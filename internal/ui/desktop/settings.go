package desktop

import (
	"context"
	"log/slog"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
)

var levels = []struct {
	name  string
	level slog.Level
}{{"debug", slog.LevelDebug}, {"info", slog.LevelInfo}, {"warn", slog.LevelWarn}, {"error", slog.LevelError}}

// settingsPane is the gear's pane: the application's settings, the bridge's
// restart and the doctor report, and the listener (read-only).
type settingsPane struct {
	w         *Window
	root      fyne.CanvasObject
	autostart *toggle
	silent    bool // setting the level from the settings, not the person
	level     *widget.Select
	addr, tls *textLine
	tlsRow    fyne.CanvasObject
	b         *busy
}

// action is a grouped row that is one button.
func action(text string, icon fyne.Resource, tapped func()) *widget.Button {
	b := widget.NewButtonWithIcon(text, icon, tapped)
	b.Alignment = widget.ButtonAlignLeading
	b.Importance = widget.LowImportance
	return b
}

func newSettingsPane(w *Window) *settingsPane {
	p := &settingsPane{w: w}
	p.addr = newTextLine("", theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{Monospace: true})
	p.tls = newTextLine("", theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
	p.autostart = newToggle("Start at login", p.setAutostart)
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = l.name
	}
	p.level = widget.NewSelect(names, p.setLevel)
	logs := action("Open logs folder", theme.FolderOpenIcon(), func() { p.simple(func() error { return w.set.OpenLogs() }) })
	cfg := action("Open config.toml", theme.DocumentIcon(), func() { p.simple(func() error { return w.set.OpenConfig() }) })
	restart := action("Restart bridge", theme.ViewRefreshIcon(), func() { p.simple(func() error { return w.act.Restart(context.Background()) }) })
	doc := action("Doctor report", theme.InfoIcon(), p.doctor)
	p.b = newBusy(p.autostart, logs, cfg, restart, doc)
	hint := newTextLine("set [log] level in config.toml to keep it", theme.ColorNameDisabled, theme.SizeNameCaptionText, fyne.TextStyle{})
	hint.Truncate = true
	p.tlsRow = divided(groupRow(key("TLS"), keyWidth, p.tls))
	heading := widget.NewLabelWithStyle("Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	heading.SizeName = theme.SizeNameSubHeadingText
	p.root = container.NewVScroll(pad(4, 24, 28, 28, vbox(18,
		heading, p.b.label,
		group("GENERAL",
			groupRow(key("Start at login"), 0, nil, vcenter(p.autostart)),
			divided(groupRow(key("Log level"), keyWidth, pad(0, 0, 0, 8, hint), vcenter(p.level))),
		),
		group("BRIDGE",
			pad(2, 2, 4, 4, restart),
			divided(pad(2, 2, 4, 4, doc)),
			divided(pad(2, 2, 4, 4, logs)),
			divided(pad(2, 2, 4, 4, cfg)),
		),
		group("LISTENER",
			groupRow(key("Address"), keyWidth, p.addr),
			p.tlsRow,
		),
	)))
	return p
}

func (p *settingsPane) open(ln *ctl.Listener) {
	p.autostart.SetOn(p.w.set.Autostart())
	p.silent = true
	cur := p.w.set.Level()
	p.level.SetSelected("")
	for _, l := range levels {
		if l.level == cur {
			p.level.SetSelected(l.name)
		}
	}
	p.silent = false
	p.setListener(ln)
}

func (p *settingsPane) setListener(ln *ctl.Listener) {
	if ln == nil {
		p.addr.SetText("The bridge is not listening.")
		p.tlsRow.Hide()
		return
	}
	p.addr.SetText(ln.Addr)
	p.tlsRow.Show()
	if ln.TLS {
		p.tls.SetText("on")
	} else {
		p.tls.SetText("off")
	}
}

func (p *settingsPane) setAutostart(on bool) {
	p.w.run(p.b, "Working…", func(context.Context) error { return p.w.set.SetAutostart(on) }, func(err error) {
		if err != nil {
			p.w.showErr(err)
			var cur bool
			p.w.async(func() { cur = p.w.set.Autostart() }, func() { p.autostart.SetOn(cur) })
		}
	})
}

func (p *settingsPane) setLevel(name string) {
	if p.silent {
		return
	}
	for _, l := range levels {
		if l.name == name {
			p.w.set.SetLevel(l.level)
		}
	}
}

func (p *settingsPane) simple(f func() error) {
	p.w.run(p.b, "Working…", func(context.Context) error { return f() }, func(err error) {
		if err != nil {
			p.w.showErr(err)
		}
	})
}

func (p *settingsPane) doctor() {
	var res []doctor.Result
	p.w.run(p.b, "Working…", func(ctx context.Context) (err error) {
		res, err = p.w.act.Doctor(ctx)
		return err
	}, func(err error) {
		if err != nil {
			p.w.showErr(err)
			return
		}
		rows := make([]fyne.CanvasObject, 0, len(res))
		for _, r := range res {
			line := "[" + string(r.Status) + "] " + r.Name + ": " + r.Message
			if r.Fix != "" {
				line += "\nFix: " + r.Fix
			}
			rows = append(rows, wrapped(line))
		}
		sc := container.NewVScroll(container.NewVBox(rows...))
		sc.SetMinSize(fyne.NewSize(500, 300))
		dialog.NewCustom("Doctor report", "Close", sc, p.w.win).Show()
	})
}
