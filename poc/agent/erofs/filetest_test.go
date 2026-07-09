package erofs

import (
	"os"
	"testing"
)

// TestConvertFile converts EROFS_TEST_TAR to EROFS_TEST_OUT (manual/CI tool
// as much as test: the real verification is mounting the result in a VM).
func TestConvertFile(t *testing.T) {
	in, out := os.Getenv("EROFS_TEST_TAR"), os.Getenv("EROFS_TEST_OUT")
	if in == "" || out == "" {
		t.Skip("EROFS_TEST_TAR/EROFS_TEST_OUT not set")
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	o, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := Convert(f, o); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	t.Logf("wrote %s: %d bytes", out, st.Size())
}
