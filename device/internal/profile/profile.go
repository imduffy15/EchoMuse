// Package profile describes the audio hardware of a supported device.
//
// Before this existed, card/device numbers, channel counts and the mixer init
// sequence were compile-time constants scattered across the bindings and
// start_server.sh, all of them specific to the Echo Dot gen 2 ("biscuit").
// Supporting a second device means naming those values and selecting them at
// runtime.
package profile

import (
	"log"
	"os/exec"
	"strings"

	"github.com/wilbowes/EchoMuse/internal/alsa"
)

// Mic describes the capture stream.
type Mic struct {
	Card, Device int
	Channels     int
	Format       alsa.Format
	SampleRate   int
	PeriodSize   int
	Periods      int

	// MicChannels are the channel indices carrying microphone audio.
	MicChannels []int
	// RefChannels carry a hardware loopback of the playback signal, suitable
	// as an AEC reference. Empty when the device provides no such feed.
	RefChannels []int
	// WakeChannel is the channel handed to wake-word detection when
	// beamforming is disabled or has not locked.
	WakeChannel int
}

// FrameBytes is the size of one interleaved frame across all channels.
func (m Mic) FrameBytes() int { return m.Channels * m.Format.Bytes() }

// Speaker describes the playback stream.
type Speaker struct {
	Card, Device int
	Channels     int
	Format       alsa.Format
	SampleRate   int
	PeriodSize   int
	Periods      int
}

// FrameBytes is the size of one interleaved output frame.
func (s Speaker) FrameBytes() int { return s.Channels * s.Format.Bytes() }

// Profile is the full hardware description for one device.
type Profile struct {
	// Name matches ro.product.device.
	Name string

	Mic     Mic
	Speaker Speaker

	// MixerInit is applied once at startup, before any PCM is opened.
	MixerInit []alsa.Control
	// AmpOff silences the output stage. Applied when the server shuts down or
	// when playback is idle, so an enabled amp on an idle DAC does not hiss.
	AmpOff []alsa.Control

	// HasLEDRing is false on devices with a screen and no LED ring; the null
	// LED controller is used instead.
	HasLEDRing bool
	// Beamforming is only meaningful with enough mics to steer. With two mics
	// there is no array to speak of and the beamformer is bypassed.
	Beamforming bool

	// StopServices are init services stopped at startup so the daemon owns the
	// audio hardware outright.
	StopServices []string
}

// HasAECReference reports whether the capture stream carries a hardware
// loopback of the playback signal.
func (p Profile) HasAECReference() bool { return len(p.Mic.RefChannels) > 0 }

// biscuit is the Echo Dot gen 2: 4x TLV320ADC3101 feeding an FPGA that
// presents 9 channels over SPI (7 mics used, 2 unconnected), 12-LED ring,
// mono speaker. Values carried over from the original bindings.
var biscuit = Profile{
	Name: "biscuit",
	Mic: Mic{
		Card: 0, Device: 24,
		Channels:   9,
		Format:     alsa.FormatS24_3LE,
		SampleRate: 16000,
		PeriodSize: 512,
		Periods:    5,
		// 6 perimeter mics plus a centre omni; ch7/ch8 are unconnected.
		MicChannels: []int{0, 1, 2, 3, 4, 5, 6},
		RefChannels: nil,
		WakeChannel: 6, // centre omni
	},
	Speaker: Speaker{
		Card: 0, Device: 23,
		Channels:   2,
		Format:     alsa.FormatS16LE,
		SampleRate: 48000,
		PeriodSize: 2048,
		Periods:    4,
	},
	MixerInit: []alsa.Control{
		{Index: 56, Values: []string{"On"}},
		{Index: 64, Values: []string{"1", "1"}},
		{Index: 88, Values: []string{"On"}},
		{Index: 61, Values: []string{"100", "100"}},
		// Mic gain, equalised across all four ADCs (A/B/C/D).
		{Index: 89, Values: []string{"88", "88"}},
		{Index: 92, Values: []string{"40", "40"}},
		{Index: 107, Values: []string{"88", "88"}},
		{Index: 110, Values: []string{"40", "40"}},
		{Index: 125, Values: []string{"88", "88"}},
		{Index: 128, Values: []string{"40", "40"}},
		{Index: 143, Values: []string{"88", "88"}},
		{Index: 146, Values: []string{"40", "40"}},
	},
	AmpOff: []alsa.Control{
		{Index: 61, Values: []string{"0", "0"}, Optional: true},
		{Index: 5, Values: []string{"Off"}, Optional: true},
	},
	HasLEDRing:   true,
	Beamforming:  true,
	StopServices: []string{"mixer", "ledcontroller"},
}

// checkers is the Echo Show 5 gen 1: a single TLV320AIC3101 feeding the same
// amzn-mt-spi-pcm FPGA path as biscuit but built for 4 channels, an RT5616
// driving a mono speaker, a screen instead of an LED ring.
//
// Every value below was read off the hardware: the driver's own HW_REFINE for
// the PCM parameters, and the Android HAL's mixer state captured at the moment
// it opened the output for the playback settings.
var checkers = Profile{
	Name: "checkers",
	Mic: Mic{
		Card: 0, Device: 22,
		Channels:    4, // channels_min == channels_max, fixed by the driver
		Format:      alsa.FormatS24_3LE,
		SampleRate:  16000,
		PeriodSize:  257, // driver minimum: 3084 bytes / 12 bytes per frame
		Periods:     4,
		MicChannels: []int{0, 1},
		// ch2/ch3 carry a bit-identical loopback of the playback signal,
		// resampled to 16 kHz by the FPGA and sample-aligned with the mics.
		// Measured echo path: 2.5 ms delay, -13 dB, 0.83 correlation.
		RefChannels: []int{2, 3},
		WakeChannel: 0,
	},
	Speaker: Speaker{
		Card: 0, Device: 23,
		Channels:   2,
		Format:     alsa.FormatS16LE,
		SampleRate: 48000,
		PeriodSize: 1536, // matches what the Amazon HAL negotiates
		Periods:    2,
	},
	MixerInit: []alsa.Control{
		// Counter-intuitive but load-bearing: the external speaker amp switch
		// must be OFF for audio to reach the speaker. Its boot default is On,
		// which silences output entirely. Found by diffing the mixer against
		// the Android HAL at the instant it opened the output PCM.
		{Name: "Ext_Speaker_Amp_Switch", Values: []string{"Off"}},
		// Mic gain, mirroring biscuit's per-ADC settings for the single ADC.
		{Name: "ADC_A Digital Volume Control", Values: []string{"88", "88"}},
		{Name: "ADC_A MICPGA Volume Ctrl", Values: []string{"40", "40"}},
	},
	AmpOff: []alsa.Control{
		{Name: "Ext_Speaker_Amp_Switch", Values: []string{"On"}, Optional: true},
	},
	HasLEDRing: false,
	// Two mics is not an array worth steering.
	Beamforming: false,
	// LineageOS rather than Fire OS: there is no "mixer" or "ledcontroller".
	// Stopping these keeps the Android HAL off the PCMs, which also keeps the
	// AEC reference valid by ensuring nothing else drives the DAC.
	StopServices: []string{"audioserver", "vendor.audio-hal"},
}

var profiles = map[string]*Profile{
	biscuit.Name:  &biscuit,
	checkers.Name: &checkers,
}

// Detect selects a profile from ro.product.device, falling back to biscuit so
// existing deployments behave exactly as before.
func Detect() *Profile {
	name := prop("ro.product.device")
	if p, ok := profiles[name]; ok {
		log.Printf("profile: detected %q", p.Name)
		return p
	}
	if name != "" {
		log.Printf("profile: unknown device %q, falling back to %q", name, biscuit.Name)
	}
	return &biscuit
}

// ByName returns a named profile, or nil if unknown. Used by the -profile
// override so a device can be forced during bring-up.
func ByName(name string) *Profile { return profiles[name] }

// Names lists the supported profiles.
func Names() []string {
	out := make([]string, 0, len(profiles))
	for n := range profiles {
		out = append(out, n)
	}
	return out
}

func prop(key string) string {
	out, err := exec.Command("getprop", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
