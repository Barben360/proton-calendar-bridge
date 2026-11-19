package update_test

import (
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

const line = "curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh"

// install is a Linux install of pcb at exe whose other files are present.
func install(goos, exe string, present ...string) update.Install {
	files := map[string]bool{}
	for _, p := range present {
		files[p] = true
	}
	return update.Install{GOOS: goos, Exe: exe, Home: "/home/u", Env: func(string) string { return "" }, Exists: func(p string) bool { return files[p] }}
}

func TestHint(t *testing.T) {
	for name, c := range map[string]struct {
		in   update.Install
		want string
	}{
		"default prefix, full install": {install("linux", "/home/u/.local/bin/pcb", "/home/u/.local/bin/pcb-ui", "/home/u/.local/share/applications/pcb-ui.desktop"), line},
		"from pcb-ui itself":           {install("linux", "/home/u/.local/bin/pcb-ui", "/home/u/.local/bin/pcb-ui", "/home/u/.local/share/applications/pcb-ui.desktop"), line},
		"other prefix":                 {install("linux", "/opt/pcb/bin/pcb", "/opt/pcb/bin/pcb-ui", "/opt/pcb/share/applications/pcb-ui.desktop"), line + " -s -- --prefix /opt/pcb"},
		"prefix with a space":          {install("linux", "/opt/my pcb/bin/pcb", "/opt/my pcb/bin/pcb-ui", "/opt/my pcb/share/applications/pcb-ui.desktop"), line + " -s -- --prefix '/opt/my pcb'"},
		"no pcb-ui":                    {install("linux", "/home/u/.local/bin/pcb"), line + " -s -- --no-ui"},
		"pcb-ui without desktop entry": {install("linux", "/home/u/.local/bin/pcb", "/home/u/.local/bin/pcb-ui"), line + " -s -- --no-desktop"},
		"other prefix, no pcb-ui":      {install("linux", "/srv/bin/pcb"), line + " -s -- --prefix /srv --no-ui"},
		"macOS gets pcb only anyway":   {install("darwin", "/home/u/.local/bin/pcb"), line},
		"not under a bin directory":    {install("linux", "/usr/local/lib/pcb/pcb"), ""},
		"Windows":                      {install("windows", `C:\Users\u\AppData\Local\Programs\pcb\bin\pcb.exe`), ""},
		"no executable path":           {install("linux", ""), ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := update.Hint(c.in); got != c.want {
				t.Errorf("Hint = %q\nwant   %q", got, c.want)
			}
		})
	}
}

func TestHintInTheContainer(t *testing.T) {
	in := install("linux", "/usr/local/bin/pcb")
	in.Env = func(k string) string {
		if k == "PCB_INSTALL" {
			return "container"
		}
		return ""
	}
	if got, want := update.Hint(in), "pull the new image: docker compose pull && docker compose up -d"; got != want {
		t.Errorf("Hint = %q, want %q", got, want)
	}
}

func TestInstallKind(t *testing.T) {
	container := install("linux", "/usr/local/bin/pcb")
	container.Env = func(k string) string {
		if k == "PCB_INSTALL" {
			return "container"
		}
		return ""
	}
	msi := func(exe string) update.Install {
		in := install("windows", exe)
		in.Env = func(k string) string {
			if k == "LOCALAPPDATA" {
				return `C:\Users\u\AppData\Local`
			}
			return ""
		}
		return in
	}
	for name, c := range map[string]struct {
		in   update.Install
		want string
	}{
		"container":                     {container, "container"},
		"install.sh, default prefix":    {install("linux", "/home/u/.local/bin/pcb", "/home/u/.local/bin/pcb-ui"), "script"},
		"install.sh, other prefix":      {install("linux", "/opt/pcb/bin/pcb"), "script"},
		"install.sh on macOS":           {install("darwin", "/home/u/.local/bin/pcb"), "script"},
		"MSI":                           {msi(`C:\Users\u\AppData\Local\Programs\pcb\pcb.exe`), "msi"},
		"MSI, other case and slashes":   {msi(`c:/users/u/appdata/local/programs/PCB/pcb-ui.exe`), "msi"},
		"Windows, elsewhere":            {msi(`C:\tools\pcb\pcb.exe`), "other"},
		"Windows, under the MSI folder": {msi(`C:\Users\u\AppData\Local\Programs\pcb\old\pcb.exe`), "other"},
		"Windows without LOCALAPPDATA":  {install("windows", `\Programs\pcb\pcb.exe`), "other"},
		"Linux, not under a bin":        {install("linux", "/usr/local/lib/pcb/pcb"), "other"},
		"no executable path":            {install("linux", ""), "other"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := update.InstallKind(c.in); got != c.want {
				t.Errorf("InstallKind = %q, want %q", got, c.want)
			}
		})
	}
}
