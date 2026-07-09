// Package erofs writes EROFS filesystem images directly from a tar stream:
// no temporary directory, no external tools (mkfs.erofs is not a thing on
// macOS), no dependencies beyond the standard library. It targets the
// uncompressed on-disk format as read by Linux v6.12 (fs/erofs/erofs_fs.h):
// extended (64-byte) inodes, flat-plain data layout, no xattrs, no
// compression — boring on purpose. EROFS is a real Linux filesystem, so
// images served from it give guests case-sensitivity, faithful ownership,
// and real modes, none of which survive a virtiofs view of APFS.
//
// Layout produced, in write order (single pass over the tar for file data;
// only metadata is held in memory):
//
//	block 0                superblock (at offset 1024, written last)
//	blocks 1..             file + symlink data, streamed from the tar
//	then                   directory dirent blocks
//	then                   inode table (the "meta area")
//
// Format notes that matter and are easy to get wrong:
//   - nid = (byte offset - meta_blkaddr*blksz) / 32; the root nid must fit
//     in the superblock's le16, so the root inode is written first.
//   - Directory lookup binary-searches dirents (fs/erofs/namei.c), so
//     entries MUST be sorted bytewise, within and across blocks, and no
//     dirent's name may span a block boundary.
//   - dirblkbits must be zero on v6.12 (the kernel rejects anything else).
//   - The superblock checksum is only verified when the SB_CHKSUM feature
//     bit is set; we set feature_compat=0 and skip it.
package erofs

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

const (
	blockSize   = 4096
	superOffset = 1024
	magic       = 0xE0F5E1E2

	// i_format: bit 0 = extended inode layout, bits 1-3 = datalayout.
	// FLAT_PLAIN is datalayout 0, so extended+plain is just 1.
	formatExtendedPlain = 1

	inodeSize = 64 // extended inode
	slotSize  = 32 // nid granularity

	// erofs_dirent file_type values (match FT_*).
	ftReg     = 1
	ftDir     = 2
	ftChardev = 3
	ftBlkdev  = 4
	ftFifo    = 5
	ftSock    = 6
	ftSymlink = 7
)

type node struct {
	name     string // final path element
	parent   *node
	children map[string]*node // dirs only

	mode  uint16 // S_IFMT | permissions
	uid   uint32
	gid   uint32
	mtime int64
	rdev  uint32

	size     int64  // file/symlink data length; dir data length once built
	blkaddr  uint32 // first data block (files, symlinks, dirs)
	nlink    uint32
	ftype    byte
	nid      uint64
	ino      uint32
	hardTo   *node // hardlink alias target (canonical inode)
}

func (n *node) isDir() bool { return n.ftype == ftDir }

// canonical follows hardlink aliases to the real inode.
func (n *node) canonical() *node {
	for n.hardTo != nil {
		n = n.hardTo
	}
	return n
}

type writer struct {
	out      io.WriteSeeker
	nextBlk  uint32 // next free data block
	inoSeq   uint32
	root     *node
	byPath   map[string]*node
}

// Convert reads a (flattened, whiteout-applied) tar stream and writes an
// EROFS image. File contents are streamed straight to out; only metadata is
// buffered.
func Convert(tr io.Reader, out io.WriteSeeker) error {
	w := &writer{
		out:     out,
		nextBlk: 1, // block 0 belongs to the superblock
		byPath:  map[string]*node{},
	}
	w.root = &node{name: "", mode: 0o755 | 0x4000, ftype: ftDir, children: map[string]*node{}, nlink: 2}
	w.byPath["."] = w.root

	if _, err := out.Seek(blockSize, io.SeekStart); err != nil {
		return err
	}

	t := tar.NewReader(tr)
	for {
		hdr, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		if err := w.addEntry(hdr, t); err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}

	w.assignNids()
	if err := w.writeDirBlocks(); err != nil {
		return err
	}
	metaBlk, inos, err := w.writeInodes()
	if err != nil {
		return err
	}
	return w.writeSuperblock(metaBlk, inos)
}

// lookup finds or creates the node for a cleaned path, creating implicit
// parent directories (tars do not always list parents first, or at all).
func (w *writer) lookup(p string, create bool) (*node, error) {
	p = path.Clean(strings.TrimPrefix(p, "/"))
	if p == "." || p == "" {
		return w.root, nil
	}
	if n, ok := w.byPath[p]; ok {
		return n, nil
	}
	if !create {
		return nil, fmt.Errorf("path %q not found", p)
	}
	parent, err := w.lookup(path.Dir(p), true)
	if err != nil {
		return nil, err
	}
	parent = parent.canonical()
	if !parent.isDir() {
		return nil, fmt.Errorf("parent of %q is not a directory", p)
	}
	base := path.Base(p)
	if len(base) > 255 {
		return nil, fmt.Errorf("name too long")
	}
	n := &node{name: base, parent: parent, mode: 0o755 | 0x4000, ftype: ftDir,
		children: map[string]*node{}, nlink: 2}
	parent.children[base] = n
	parent.nlink++ // subdir's ".." (assume dir until told otherwise)
	w.byPath[p] = n
	return n, nil
}

func (w *writer) addEntry(hdr *tar.Header, r io.Reader) error {
	n, err := w.lookup(hdr.Name, true)
	if err != nil {
		return err
	}
	// A pre-created implicit dir being replaced by a non-dir entry.
	if n.isDir() && hdr.Typeflag != tar.TypeDir {
		if len(n.children) > 0 {
			return fmt.Errorf("non-directory tar entry over populated directory")
		}
		n.parent.nlink-- // no longer a subdir
		n.children = nil
		n.nlink = 1
	}

	n.uid = uint32(hdr.Uid)
	n.gid = uint32(hdr.Gid)
	n.mtime = hdr.ModTime.Unix()
	perm := uint16(hdr.Mode & 0o7777)

	switch hdr.Typeflag {
	case tar.TypeDir:
		n.mode = perm | 0x4000
		n.ftype = ftDir
		if n.children == nil {
			n.children = map[string]*node{}
		}
	case tar.TypeReg:
		n.mode = perm | 0x8000
		n.ftype = ftReg
		n.nlink = 1
		return w.streamData(n, r, hdr.Size)
	case tar.TypeSymlink:
		n.mode = perm | 0xA000
		n.ftype = ftSymlink
		n.nlink = 1
		return w.streamData(n, strings.NewReader(hdr.Linkname), int64(len(hdr.Linkname)))
	case tar.TypeLink:
		target, err := w.lookup(hdr.Linkname, false)
		if err != nil {
			return fmt.Errorf("hardlink target: %w", err)
		}
		target = target.canonical()
		n.hardTo = target
		target.nlink++
	case tar.TypeChar:
		n.mode = perm | 0x2000
		n.ftype = ftChardev
		n.nlink = 1
		n.rdev = mkdev(hdr.Devmajor, hdr.Devminor)
	case tar.TypeBlock:
		n.mode = perm | 0x6000
		n.ftype = ftBlkdev
		n.nlink = 1
		n.rdev = mkdev(hdr.Devmajor, hdr.Devminor)
	case tar.TypeFifo:
		n.mode = perm | 0x1000
		n.ftype = ftFifo
		n.nlink = 1
	default:
		// XGlobalHeader etc: ignore.
	}
	return nil
}

func mkdev(major, minor int64) uint32 {
	// Linux new_encode_dev
	return uint32(minor&0xff) | uint32(major<<8) | uint32((minor&^0xff)<<12)
}

// streamData copies content to the current data position, block-aligned.
func (w *writer) streamData(n *node, r io.Reader, size int64) error {
	n.size = size
	n.blkaddr = w.nextBlk
	if size == 0 {
		n.blkaddr = 0
		return nil
	}
	written, err := io.Copy(w.out, r)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("short data: %d of %d bytes", written, size)
	}
	return w.padToBlock(size)
}

func (w *writer) padToBlock(size int64) error {
	blocks := (size + blockSize - 1) / blockSize
	if pad := blocks*blockSize - size; pad > 0 {
		if _, err := w.out.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	w.nextBlk += uint32(blocks)
	return nil
}

// walk visits every canonical inode, root first, then breadth-first in
// sorted order (deterministic output).
func (w *writer) walk(fn func(*node)) {
	queue := []*node{w.root}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		fn(n)
		if n.isDir() {
			names := make([]string, 0, len(n.children))
			for name := range n.children {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				child := n.children[name]
				if child.hardTo == nil {
					queue = append(queue, child)
				}
			}
		}
	}
}

func (w *writer) assignNids() {
	// Slot 0 is left unused (nid 0 stays "nothing"); inodes are 64 bytes =
	// 2 slots each, so they never straddle a block (4096/64 exactly).
	var next uint64 = 2
	w.walk(func(n *node) {
		w.inoSeq++
		n.ino = w.inoSeq
		n.nid = next
		next += inodeSize / slotSize
	})
}

// writeDirBlocks materializes every directory's dirent data.
func (w *writer) writeDirBlocks() error {
	var err error
	w.walk(func(n *node) {
		if err != nil || !n.isDir() {
			return
		}
		err = w.writeOneDir(n)
	})
	return err
}

type dirent struct {
	name  string
	nid   uint64
	ftype byte
}

func (w *writer) writeOneDir(n *node) error {
	parent := n.parent
	if parent == nil {
		parent = n // root: ".." is itself
	}
	entries := []dirent{
		{".", n.nid, ftDir},
		{"..", parent.nid, ftDir},
	}
	for name, child := range n.children {
		c := child.canonical()
		entries = append(entries, dirent{name, c.nid, c.ftype})
	}
	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare([]byte(entries[i].name), []byte(entries[j].name)) < 0
	})

	// Pack sorted entries into blocks: 12 bytes per dirent + the name, all
	// within one block.
	var blocks [][]byte
	i := 0
	for i < len(entries) {
		count, used := 0, 0
		for j := i; j < len(entries); j++ {
			need := used + 12 + len(entries[j].name)
			if need > blockSize {
				break
			}
			used = need
			count++
		}
		if count == 0 {
			return fmt.Errorf("dirent %q does not fit a block", entries[i].name)
		}
		blk := make([]byte, blockSize)
		nameoff := count * 12
		for k := 0; k < count; k++ {
			e := entries[i+k]
			binary.LittleEndian.PutUint64(blk[k*12:], e.nid)
			binary.LittleEndian.PutUint16(blk[k*12+8:], uint16(nameoff))
			blk[k*12+10] = e.ftype
			copy(blk[nameoff:], e.name)
			nameoff += len(e.name)
		}
		blocks = append(blocks, blk[:nameoff])
		i += count
	}

	n.blkaddr = w.nextBlk
	var total int64
	for bi, blk := range blocks {
		if _, err := w.out.Write(blk); err != nil {
			return err
		}
		if bi < len(blocks)-1 {
			// Intermediate blocks are padded to a full block and counted
			// fully in i_size (the reader trims padding via strnlen).
			if _, err := w.out.Write(make([]byte, blockSize-len(blk))); err != nil {
				return err
			}
			total += blockSize
		} else {
			total += int64(len(blk))
		}
	}
	n.size = total
	return w.padToBlock(total)
}

// writeInodes lays down the meta area and returns (meta_blkaddr, inos).
func (w *writer) writeInodes() (uint32, uint64, error) {
	metaBlk := w.nextBlk
	buf := make([]byte, inodeSize)
	var inos uint64
	var err error
	// Slot 0/1 padding so the first inode sits at nid 2.
	if _, e := w.out.Write(make([]byte, 2*slotSize)); e != nil {
		return 0, 0, e
	}
	written := int64(2 * slotSize)
	w.walk(func(n *node) {
		if err != nil {
			return
		}
		inos++
		for i := range buf {
			buf[i] = 0
		}
		binary.LittleEndian.PutUint16(buf[0:], formatExtendedPlain)
		binary.LittleEndian.PutUint16(buf[4:], n.mode)
		binary.LittleEndian.PutUint64(buf[8:], uint64(n.size))
		switch n.ftype {
		case ftChardev, ftBlkdev:
			binary.LittleEndian.PutUint32(buf[16:], n.rdev)
		default:
			binary.LittleEndian.PutUint32(buf[16:], n.blkaddr)
		}
		binary.LittleEndian.PutUint32(buf[20:], n.ino)
		binary.LittleEndian.PutUint32(buf[24:], n.uid)
		binary.LittleEndian.PutUint32(buf[28:], n.gid)
		binary.LittleEndian.PutUint64(buf[32:], uint64(n.mtime))
		binary.LittleEndian.PutUint32(buf[44:], n.nlink)
		if _, e := w.out.Write(buf); e != nil {
			err = e
		}
		written += inodeSize
	})
	if err != nil {
		return 0, 0, err
	}
	if e := w.padToBlock(written); e != nil {
		return 0, 0, e
	}
	return metaBlk, inos, nil
}

func (w *writer) writeSuperblock(metaBlk uint32, inos uint64) error {
	sb := make([]byte, 128)
	binary.LittleEndian.PutUint32(sb[0:], magic)
	// checksum(4)=0, feature_compat(8)=0: SB_CHKSUM unset, so not verified
	sb[12] = 12 // blkszbits
	binary.LittleEndian.PutUint16(sb[14:], uint16(w.root.nid))
	binary.LittleEndian.PutUint64(sb[16:], inos)
	binary.LittleEndian.PutUint32(sb[36:], w.nextBlk) // blocks (statfs only)
	binary.LittleEndian.PutUint32(sb[40:], metaBlk)
	copy(sb[48:64], "kube-on-macos---") // uuid: fixed, images are content-addressed upstream
	copy(sb[64:80], "pod-image")
	if _, err := w.out.Seek(superOffset, io.SeekStart); err != nil {
		return err
	}
	_, err := w.out.Write(sb)
	return err
}
