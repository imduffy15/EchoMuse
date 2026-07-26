// Package cue gives wake-word acknowledgement to devices without an LED ring.
//
// On biscuit the 12-LED ring is the feedback: it turns green the instant the
// controller starts a voice turn, so you know you were heard before you start
// speaking. A device with a screen and no ring has nothing, and without any
// acknowledgement you end up talking over a device that is not listening.
//
// Rather than invent a new control message, this hangs off the signal the
// device already receives — the controller's explicit "this frame is the
// listening ring" hint — and renders it with whatever the hardware does have:
// a rising chime when listening starts, a falling one when it ends, and the
// screen backlight held bright in between.
package cue

import (
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wilbowes/EchoMuse/internal/profile"
)

// Player is the slice of the speaker the chime needs.
type Player interface {
	PumpPeriod(data []byte) error
	EndStream()
}

// Cue renders wake acknowledgement.
type Cue struct {
	prof *profile.Profile
	spk  Player

	// Tones are pre-rendered once at construction: they never change, and
	// synthesising one on the wake path would add latency to the moment that
	// has to feel instant.
	chimeUp   []byte
	chimeDown []byte

	mu            sync.Mutex
	listening     bool
	prevBacklight string
	// turn distinguishes successive turns so a late safety-net timer from an
	// earlier turn cannot restore the screen during a later one.
	turn uint64
}

// maxTurn bounds how long the screen may stay held bright without an
// end-of-listening signal.
const maxTurn = 60 * time.Second

// New builds a Cue for the profile. Returns nil when the device has an LED
// ring, since the ring already is the acknowledgement.
func New(p *profile.Profile, spk Player) *Cue {
	if !p.WakeCue.Enabled {
		return nil
	}
	c := &Cue{prof: p, spk: spk}
	if p.WakeCue.ChimeMs > 0 {
		c.chimeUp = renderChime(p, true)
		c.chimeDown = renderChime(p, false)
	}
	return c
}

// Start acknowledges the beginning of a voice turn: a rising chime, and the
// screen held bright for as long as the device is listening.
//
// The screen is held rather than blipped because a flash only answers "did it
// hear me", not "is it still listening" — and the second question is the one
// you need answered while deciding whether to keep talking.
//
// Safe to call from the control-plane goroutine: it returns immediately and
// does the work in the background, so a slow sysfs write or a full audio queue
// can never stall LED handling.
func (c *Cue) Start() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.listening {
		c.mu.Unlock()
		return // already acknowledged this turn
	}
	c.listening = true
	c.turn++
	turn := c.turn
	c.mu.Unlock()

	go func() {
		c.raiseBacklight()
		c.playTone(c.chimeUp)

		// Safety net: if the falling edge never arrives — a controller that
		// drops mid-turn, a turn that times out — the screen would stay at
		// full brightness indefinitely. Restore it ourselves after a bound
		// well past any real turn.
		time.Sleep(maxTurn)
		c.mu.Lock()
		stale := c.listening && c.turn == turn
		if stale {
			c.listening = false
		}
		c.mu.Unlock()
		if stale {
			log.Printf("cue: no end-of-turn after %v — restoring screen", maxTurn)
			c.restoreBacklight()
		}
	}()
}

// Stop acknowledges the end of listening: a falling chime, and the screen back
// to whatever it was.
func (c *Cue) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.listening {
		c.mu.Unlock()
		return
	}
	c.listening = false
	c.mu.Unlock()

	go func() {
		c.playTone(c.chimeDown)
		c.restoreBacklight()
	}()
}

// raiseBacklight stores the current brightness and goes to full. The previous
// value is read at Start rather than cached at construction so this works
// whatever the user or Android left behind, including a screen that had
// dimmed or switched off.
func (c *Cue) raiseBacklight() {
	path := c.prof.WakeCue.BacklightPath
	if path == "" {
		return
	}
	prev, err := os.ReadFile(path)
	if err != nil {
		log.Printf("cue: read backlight: %v", err)
		return
	}
	c.mu.Lock()
	c.prevBacklight = strings.TrimSpace(string(prev))
	c.mu.Unlock()

	max := c.prof.WakeCue.BacklightMax
	if max <= 0 {
		max = 255
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(max)), 0644); err != nil {
		log.Printf("cue: write backlight: %v", err)
	}
}

// restoreBacklight puts back the brightness captured at Start.
func (c *Cue) restoreBacklight() {
	path := c.prof.WakeCue.BacklightPath
	if path == "" {
		return
	}
	c.mu.Lock()
	prev := c.prevBacklight
	c.mu.Unlock()
	if prev == "" {
		return
	}
	if err := os.WriteFile(path, []byte(prev), 0644); err != nil {
		log.Printf("cue: restore backlight: %v", err)
	}
}

// playChime pushes the pre-rendered tone through the normal playback path, so
// it goes through the same mono-to-stereo duplication, priming and AEC tap as
// TTS — the chime therefore lands in the echo reference too and does not
// train the canceller on a signal it never saw.
func (c *Cue) playTone(tone []byte) {
	if len(tone) == 0 {
		return
	}
	c.pump(tone)
}

func (c *Cue) pump(chime []byte) {
	frameBytes := 2 // mono S16
	period := c.prof.Speaker.PeriodSize * frameBytes
	for off := 0; off < len(chime); off += period {
		end := off + period
		if end > len(chime) {
			end = len(chime)
		}
		buf := make([]byte, period) // pad the tail with silence
		copy(buf, chime[off:end])
		if err := c.spk.PumpPeriod(buf); err != nil {
			log.Printf("cue: chime: %v", err)
			return
		}
	}
	c.spk.EndStream()
}

// renderChime synthesises the acknowledgement tone as mono S16 at the
// speaker's rate.
//
// It is a two-note pair rather than a single beep: two short notes cut through
// room noise better than one longer one, and the interval direction is what
// distinguishes start from end. Both notes get a raised-
// cosine envelope — a bare sine switched on and off clicks, and on this device
// the click is loud enough to be the thing you notice instead of the tone.
func renderChime(p *profile.Profile, rising bool) []byte {
	rate := float64(p.Speaker.SampleRate)
	noteMs := p.WakeCue.ChimeMs / 2
	if noteMs <= 0 {
		noteMs = 60
	}
	amp := p.WakeCue.ChimeAmplitude
	if amp <= 0 || amp > 1 {
		amp = 0.18
	}
	base := p.WakeCue.ChimeHz
	if base <= 0 {
		base = 880
	}

	// Rising for "listening", falling for "done". Direction is what carries
	// the meaning — the ear reads a rising interval as opening and a falling
	// one as closing, so the two are distinguishable without being told.
	notes := []float64{base, base * 1.25}
	if !rising {
		notes = []float64{base * 1.25, base}
	}
	out := make([]byte, 0, int(rate*float64(noteMs*len(notes))/1000)*2)

	for _, hz := range notes {
		n := int(rate * float64(noteMs) / 1000)
		for i := 0; i < n; i++ {
			// Raised-cosine window over the whole note: zero at both ends.
			env := 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
			v := math.Sin(2*math.Pi*hz*float64(i)/rate) * env * amp
			s := int16(v * math.MaxInt16)
			out = append(out, byte(s), byte(s>>8))
		}
	}
	return out
}
