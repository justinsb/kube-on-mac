ARTIFACTS ?= ../_artifacts

podvm: podvm.c
	cc -O2 -g -o $@ $< -I$(ARTIFACTS) -L$(ARTIFACTS) -lkrun \
	   -Wl,-rpath,@executable_path/../_artifacts \
	   -Wl,-rpath,$(abspath $(ARTIFACTS))
	install_name_tool -change libkrun.2.dylib @rpath/libkrun.dylib $@ 2>/dev/null || true
	codesign --entitlements podvm.entitlements --force -s - $@

clean:
	rm -f podvm

.PHONY: clean
