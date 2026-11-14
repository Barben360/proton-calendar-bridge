package ctl

import "testing"

// The pipe's name is the hash of the lower-cased final path: one name per
// state directory whatever its case, another for another directory, and no
// path in it.
func TestPipeNameFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ final, want string }{
		{`\\?\C:\Users\u\AppData\Local\pcb`, `\\.\pipe\pcb-729bea6d7f1c2636`},
		{`\\?\c:\users\u\appdata\local\pcb`, `\\.\pipe\pcb-729bea6d7f1c2636`},
		{`\\?\C:\Users\v\AppData\Local\pcb`, `\\.\pipe\pcb-cc401da736e20f06`},
	} {
		if got := pipeNameFor(tc.final); got != tc.want {
			t.Errorf("pipeNameFor(%s) = %s, want %s", tc.final, got, tc.want)
		}
	}
}
