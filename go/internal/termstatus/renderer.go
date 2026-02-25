package termstatus

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	ModeCompact = "compact"
	ModeVerbose = "verbose"
	ModeErrors  = "errors"
)

type Renderer struct {
	mu sync.Mutex

	w    io.Writer
	mode string
	tty  bool

	progressActive bool
	lastProgress   string
	lastLineAt     time.Time
	minLineGap     time.Duration
}

func NormalizeMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ModeCompact:
		return ModeCompact
	case ModeVerbose:
		return ModeVerbose
	case ModeErrors:
		return ModeErrors
	default:
		return ""
	}
}

func New(w io.Writer, mode string, tty bool) *Renderer {
	normalized := NormalizeMode(mode)
	if normalized == "" {
		normalized = ModeCompact
	}
	return &Renderer{
		w:          w,
		mode:       normalized,
		tty:        tty,
		minLineGap: time.Second,
	}
}

func (r *Renderer) Progressf(format string, args ...interface{}) {
	if r == nil {
		return
	}
	msg := normalizeMessage(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.mode {
	case ModeVerbose:
		r.writeLine(msg)
	case ModeErrors:
		return
	default:
		r.writeProgress(msg)
	}
}

func (r *Renderer) Infof(format string, args ...interface{}) {
	if r == nil {
		return
	}
	msg := normalizeMessage(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushProgressLine()
	r.writeLine(msg)
}

func (r *Renderer) Errorf(format string, args ...interface{}) {
	if r == nil {
		return
	}
	msg := normalizeMessage(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.flushProgressLine()
	r.writeLine(msg)
}

func (r *Renderer) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushProgressLine()
}

func (r *Renderer) writeProgress(msg string) {
	if r.tty {
		fmt.Fprintf(r.w, "\r\033[2K%s", msg)
		r.progressActive = true
		r.lastProgress = msg
		return
	}
	now := time.Now()
	if msg == r.lastProgress && !r.lastLineAt.IsZero() && now.Sub(r.lastLineAt) < r.minLineGap {
		return
	}
	r.writeLine(msg)
	r.lastProgress = msg
	r.lastLineAt = now
}

func (r *Renderer) flushProgressLine() {
	if r.progressActive && r.tty {
		fmt.Fprint(r.w, "\r\033[2K")
		fmt.Fprintln(r.w)
		r.progressActive = false
	}
}

func (r *Renderer) writeLine(msg string) {
	fmt.Fprintln(r.w, msg)
}

func normalizeMessage(msg string) string {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		return ""
	}
	return strings.Join(strings.Fields(trimmed), " ")
}
