package desktop

import (
	"context"
	"log/slog"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
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
	autostart *widget.Check
	silent    bool
	level     *widget.Select
	addr, tls *widget.Label
	b         *busy
}

func newSettingsPane(w *Window) *settingsPane {
	p := &settingsPane{w: w, addr: widget.NewLabel(""), tls: widget.NewLabel("")}
	p.autostart = widget.NewCheck("Start at login", p.setAutostart)
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = l.name
	}
	p.level = widget.NewSelect(names, p.setLevel)
	logs := widget.NewButton("Open logs folder", func() { p.simple(func() error { return w.set.OpenLogs() }) })
	cfg := widget.NewButton("Open config.toml", func() { p.simple(func() error { return w.set.OpenConfig() }) })
	restart := widget.NewButton("Restart bridge", func() { p.simple(func() error { return w.act.Restart(context.Background()) }) })
	doc := widget.NewButton("Doctor report", p.doctor)
	p.b = newBusy(p.autostart, logs, cfg, restart, doc)
	p.root = container.NewVScroll(container.NewVBox(
		title("Settings"), p.autostart,
		container.NewHBox(widget.NewLabel("Log level"), p.level),
		widget.NewLabel("set [log] level in config.toml to keep it"),
		logs, cfg, restart, doc, p.b.label,
		widget.NewSeparator(), title("Listener"), p.addr, p.tls,
	))
	return p
}

func (p *settingsPane) open(ln *ctl.Listener) {
	p.silent = true
	p.autostart.SetChecked(p.w.set.Autostart())
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
		p.tls.SetText("")
		return
	}
	p.addr.SetText("Listening on " + ln.Addr)
	if ln.TLS {
		p.tls.SetText("TLS: on")
	} else {
		p.tls.SetText("TLS: off")
	}
}

func (p *settingsPane) setAutostart(on bool) {
	if p.silent {
		return
	}
	p.w.run(p.b, "Working…", func(context.Context) error { return p.w.set.SetAutostart(on) }, func(err error) {
		if err != nil {
			p.w.showErr(err)
			var cur bool
			p.w.async(func() { cur = p.w.set.Autostart() }, func() {
				p.silent = true
				p.autostart.SetChecked(cur)
				p.silent = false
			})
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
