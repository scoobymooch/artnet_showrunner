// dmx_go — Timeline‑driven DMX + audio Art‑Net player
//
// Overview
//   • Reads a YAML show config defining audio, Art‑Net targets, fixture profiles, and a patch.
//   • Loads per‑fixture timeline text files. Each line encodes: START END COMMAND [values]
//     where times can be seconds (e.g., 12.5) or MM:SS.mmm / HH:MM:SS.mmm.
//   • Supported commands:
//       set       — instant write of channel values
//       fade      — linear fade from current values to target over (END‑START)
//       temp_set  — set values for a duration, then restore snapshot
//       temp_fade — fade in → hold → fade out
//   • Audio (mp3/wav) is decoded via faiface/beep; playback is synchronised with DMX events.
//   • DMX frames (512 bytes) are sent via UDP Art‑Net (unicast) to one or more targets.
//
// Notes
//   • Fades infer starting values from the current DMX frame when not explicitly given.
//   • Color keywords (e.g., "red", "cyan") expand to RGBA‑style tuples per profile ordering.
//   • Error messages aim to be explicit about bad input formats and timing issues.

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"github.com/faiface/beep/wav"
	"gopkg.in/yaml.v3"
)

// pollArtNetNodes sends an ArtPoll and listens for ArtPollReply packets, printing node info.
func pollArtNetNodes() {
	const artnetPort = 6454
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: artnetPort})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open UDP socket on port %d: %v\n", artnetPort, err)
		os.Exit(1)
	}
	defer conn.Close()

	// Enable broadcast
	rawConn, err := conn.SyscallConn()
	if err == nil {
		rawConn.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	}

	// Build ArtPoll packet
	pkt := make([]byte, 14)
	copy(pkt[0:], []byte("Art-Net\x00"))
	pkt[8], pkt[9] = 0x00, 0x20 // OpCode ArtPoll (0x2000)
	pkt[10], pkt[11] = 0x00, 14 // ProtVerHi, ProtVerLo
	pkt[12] = 0x06              // TalkToMe flags
	pkt[13] = 0x00              // Priority

	// Find broadcast address, preferring interfaces with 192.168.* addresses
	bcastIP := net.IPv4bcast
	foundPreferred := false
	ifaces, err := net.Interfaces()
	if err == nil {
		var firstValidBcast net.IP
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ipnet *net.IPNet
				switch v := addr.(type) {
				case *net.IPNet:
					ipnet = v
				case *net.IPAddr:
					ipnet = &net.IPNet{IP: v.IP, Mask: v.IP.DefaultMask()}
				}
				if ipnet == nil || ipnet.IP == nil || ipnet.IP.To4() == nil {
					continue
				}
				ip := ipnet.IP.To4()
				mask := ipnet.Mask
				if mask == nil || len(mask) != 4 {
					continue
				}
				bcast := make(net.IP, 4)
				for i := 0; i < 4; i++ {
					bcast[i] = ip[i] | ^mask[i]
				}
				if strings.HasPrefix(ip.String(), "192.168.") {
					bcastIP = bcast
					foundPreferred = true
					break
				}
				if firstValidBcast == nil {
					firstValidBcast = bcast
				}
			}
			if foundPreferred {
				break
			}
		}
		if !foundPreferred && firstValidBcast != nil {
			bcastIP = firstValidBcast
		}
	}

	bcastAddr := &net.UDPAddr{IP: bcastIP, Port: artnetPort}
	fmt.Printf("Broadcasting ArtPoll to %s ...\n", bcastIP.String())
	if _, err := conn.WriteToUDP(pkt, bcastAddr); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to send ArtPoll: %v\n", err)
		os.Exit(1)
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	fmt.Println("Listening for ArtPollReply packets (5s)...")
	found := false
	buf := make([]byte, 512)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if n < 44 || string(buf[0:7]) != "Art-Net" || buf[8] != 0x00 || buf[9] != 0x21 {
			continue
		}
		name := buf[26:44]
		for i, b := range name {
			if b == 0 {
				name = name[:i]
				break
			}
		}
		fmt.Printf("Node: %-16s  IP: %s\n", string(name), addr.IP.String())
		found = true
	}

	if !found {
		fmt.Println("No Art-Net nodes replied.")
	}
}

// colorMap expands friendly colour names into ordered channel tuples (per profile ordering).
var colorMap = map[string][]int{
	"off":       {0, 0, 0, 0},
	"red":       {255, 255, 0, 0},
	"green":     {255, 0, 255, 0},
	"blue":      {255, 0, 0, 255},
	"white":     {255, 255, 255, 255},
	"amber":     {255, 255, 191, 0},
	"cyan":      {255, 0, 255, 255},
	"magenta":   {255, 255, 0, 255},
	"yellow":    {255, 255, 255, 0},
	"purple":    {255, 128, 0, 128},
	"pink":      {255, 255, 105, 180},
	"orange":    {255, 255, 69, 0},
	"warmwhite": {255, 255, 244, 229},
	"coldwhite": {255, 200, 255, 255},
	"gold":      {255, 255, 215, 0},
	"lime":      {255, 191, 255, 0},
	"turquoise": {255, 64, 224, 208},
	"violet":    {255, 238, 130, 238},
}

// Project represents the root YAML configuration for a show: audio path, timing offset,
// BPM (optional), fixture profiles, and the fixture patch list.
type Project struct {
	Audio           string             `yaml:"audio"`
	OffsetMS        int                `yaml:"offset_ms"`
	BPM             float64            `yaml:"bpm"`
	Profiles        map[string]Profile `yaml:"profiles"`
	Patch           []PatchedFixture   `yaml:"patch"`
	BroadcastSubnet string             `yaml:"broadcast_subnet,omitempty"`
}

// Profile maps human‑readable attribute names to 1‑based DMX channel numbers.
type Profile struct {
	Channels map[string]int `yaml:"channels"`
}

// PatchedFixture binds a fixture ID to a profile and a base address, and points to a timeline file.
type PatchedFixture struct {
	ID       string `yaml:"id"`
	Profile  string `yaml:"profile"`
	Base     int    `yaml:"base"`
	Timeline string `yaml:"timeline"`
}

// Event is the generic timeline event container; exactly one of the embedded event types is set.
type Event struct {
	At       string         `yaml:"at"`
	Set      *SetEvent      `yaml:"set,omitempty"`
	Fade     *FadeEvent     `yaml:"fade,omitempty"`
	TempSet  *TempSetEvent  `yaml:"temp_set,omitempty"`
	TempFade *TempFadeEvent `yaml:"temp_fade,omitempty"`
}

// SetEvent writes values to attributes at a specific time.
type SetEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	Values  map[string]int `yaml:"values,omitempty"`
}

// FadeEvent linearly interpolates attributes from current values to ToMap over Dur.
type FadeEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	FromMap map[string]int `yaml:"from_map,omitempty"`
	ToMap   map[string]int `yaml:"to_map,omitempty"`
	Dur     string         `yaml:"dur"`
}

// TempSetEvent applies values for Dur and then restores a snapshot of previous values.
type TempSetEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	ToMap   map[string]int `yaml:"to_map"`
	Dur     string         `yaml:"dur"`
}

// TempFadeEvent performs a fade‑in, hold, then fade‑out to the given ToMap.
type TempFadeEvent struct {
	Fixture string         `yaml:"fixture,omitempty"`
	ToMap   map[string]int `yaml:"to_map"`
	FadeIn  string         `yaml:"fade_in"`
	Hold    string         `yaml:"hold"`
	FadeOut string         `yaml:"fade_out"`
}

// must terminates the program with a user‑friendly error if err != nil.
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parseTimestampMS converts a timestamp string into milliseconds.
// Supported formats:
//   - Plain seconds: "12.5"
//   - MM:SS.mmm    : "01:23.456"
//   - HH:MM:SS.mmm : "1:02:03.004"
func parseTimestampMS(ts string) (int64, error) {
	ts = strings.TrimSpace(strings.ReplaceAll(ts, "\uFEFF", ""))
	if ts == "" {
		return 0, fmt.Errorf("empty timestamp")
	}

	// Plain seconds (no colon)
	if !strings.Contains(ts, ":") {
		if sec, err := strconv.ParseFloat(ts, 64); err == nil {
			return int64(sec * 1000), nil
		}
	}

	// Split HH:MM:SS(.mmm) or MM:SS(.mmm)
	parts := strings.Split(ts, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad time %q; use MM:SS.mmm or HH:MM:SS.mmm", ts)
	}

	var h, m int64
	var secMilli string
	if len(parts) == 3 {
		h, _ = strconv.ParseInt(parts[0], 10, 64)
		m, _ = strconv.ParseInt(parts[1], 10, 64)
		secMilli = parts[2]
	} else {
		m, _ = strconv.ParseInt(parts[0], 10, 64)
		secMilli = parts[1]
	}

	var sPart, msPart int64
	if strings.Contains(secMilli, ".") {
		sp := strings.SplitN(secMilli, ".", 2)
		sPart, _ = strconv.ParseInt(sp[0], 10, 64)
		msStr := sp[1]
		for len(msStr) < 3 {
			msStr += "0"
		}
		if len(msStr) > 3 {
			msStr = msStr[:3]
		}
		msPart, _ = strconv.ParseInt(msStr, 10, 64)
	} else {
		sPart, _ = strconv.ParseInt(secMilli, 10, 64)
	}

	return (((h*60+m)*60)+sPart)*1000 + msPart, nil
}

// decodeAudio opens and decodes an mp3 or wav file using faiface/beep.
func decodeAudio(path string) (beep.StreamSeekCloser, beep.Format, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, beep.Format{}, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3":
		stream, format, err := mp3.Decode(file)
		if err != nil {
			file.Close()
			return nil, beep.Format{}, err
		}
		return stream, format, nil
	case ".wav":
		stream, format, err := wav.Decode(file)
		if err != nil {
			file.Close()
			return nil, beep.Format{}, err
		}
		return stream, format, nil
	default:
		file.Close()
		return nil, beep.Format{}, fmt.Errorf("unsupported audio format %q", ext)
	}
}

// ArtNetSender manages UDP socket and sequence number for broadcast sending.
type ArtNetSender struct {
	conn      *net.UDPConn
	broadcast *net.UDPAddr
	seq       uint8
}

// NewArtNetSender opens a UDP socket and prepares broadcast address.
func NewArtNetSender(subnet string) (*ArtNetSender, error) {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}

	// Enable broadcast on the socket
	raw, err2 := conn.SyscallConn()
	if err2 == nil {
		raw.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
	} else {
		fmt.Println("Warning: unable to set SO_BROADCAST:", err2)
	}

	ip := net.IPv4bcast
	if subnet != "" {
		parsed := net.ParseIP(subnet)
		if parsed != nil {
			ip = parsed
		} else {
			fmt.Println("Warning: invalid broadcast_subnet; using 255.255.255.255")
		}
	}

	bcast := &net.UDPAddr{IP: ip, Port: 6454}
	fmt.Printf("Broadcasting Art-Net to %s\n", ip.String())
	return &ArtNetSender{conn: conn, broadcast: bcast, seq: 1}, nil
}

// SendFrame broadcasts a DMX frame on the subnet.
func (a *ArtNetSender) SendFrame(dmx []byte, universe uint16) error {
	if len(dmx) > 512 {
		return errors.New("dmx length must be <= 512")
	}
	packet := buildArtDMX(a.seq, universe, dmx)
	a.seq++
	n, err := a.conn.WriteToUDP(packet, a.broadcast)
	if err != nil {
		fmt.Printf("ArtDMX send error (seq=%d, target=%s, wrote=%d): %v\n", a.seq-1, a.broadcast.String(), n, err)
	} else {
		fmt.Printf("Sent ArtDMX frame seq=%d to %s (%d bytes payload)\n", a.seq-1, a.broadcast.IP.String(), n)
	}
	os.Stdout.Sync()
	return err
}

// SendArtSync broadcasts an ArtSync packet so all nodes apply buffered DMX data simultaneously.
func (a *ArtNetSender) SendArtSync() error {
	pkt := []byte("Art-Net\x00\x00\x52\x00\x0e\x00")
	n, err := a.conn.WriteToUDP(pkt, a.broadcast)
	if err != nil {
		fmt.Printf("ArtSync send error (target=%s, wrote=%d): %v\n", a.broadcast.String(), n, err)
	} else {
		fmt.Printf("Sent ArtSync to %s\n", a.broadcast.IP.String())
	}
	os.Stdout.Sync()
	return err
}

// buildArtDMX constructs an Art‑DMX packet for the given universe and payload.
func buildArtDMX(seq uint8, universe uint16, dmxPayload []byte) []byte {
	subUni := byte(universe & 0xFF)
	netHi := byte((universe >> 8) & 0x7F)
	packet := make([]byte, 18+len(dmxPayload))
	copy(packet[0:], []byte("Art-Net\x00")) // ID
	packet[8], packet[9] = 0x00, 0x50       // OpCode ArtDMX
	packet[10], packet[11] = 0x00, 14       // Protocol version 14
	packet[12], packet[13] = seq, 0x00      // Sequence, physical port (unused)
	packet[14], packet[15] = subUni, netHi  // SubUni, Net
	dataLen := len(dmxPayload)
	packet[16], packet[17] = byte((dataLen>>8)&0xFF), byte(dataLen&0xFF)
	copy(packet[18:], dmxPayload)
	return packet
}

// runFrameLoop sends DMX + ArtSync at a stable 40 Hz frame rate.
func runFrameLoop(s *ArtNetSender, dmx []byte, univ uint16) {
	fmt.Println("Starting 40 Hz Art-Net frame loop...")
	// pre-roll: get frame N into node buffers
	// s.SendFrame(dmx, univ)
	// fmt.Println("Pre-roll frame sent")
	t := time.NewTicker(25 * time.Millisecond)
	for range t.C {
		// s.SendArtSync()                  // latch frame N
		// time.Sleep(1 * time.Millisecond) // small gap (2–3 ms on Wi-Fi)
		s.SendFrame(dmx, univ) // send frame N+1 for next tick
	}
}

// loadTimelines parses all per‑fixture timeline files into a flat, time‑stamped Event list.
func loadTimelines(proj *Project) ([]Event, error) {
	var events []Event
	timelineLineRE := regexp.MustCompile(`^([0-9.:\s]+)\s+([0-9.:\s]+)\s+(\w+)\s*\[(.*?)\]`)

	for _, fixture := range proj.Patch {
		if fixture.Timeline == "" {
			continue
		}

		fileData, err := os.ReadFile(fixture.Timeline)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", fixture.Timeline, err)
		}

		profile := proj.Profiles[fixture.Profile]

		// Order attributes by their channel number (not alphabetically).
		type chAttr struct {
			name string
			ch   int
		}
		var sorted []chAttr
		for name, ch := range profile.Channels {
			sorted = append(sorted, chAttr{name, ch})
		}
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ch < sorted[j].ch })
		orderedAttrs := make([]string, 0, len(sorted))
		for _, it := range sorted {
			orderedAttrs = append(orderedAttrs, it.name)
		}

		for lineIdx, rawLine := range strings.Split(string(fileData), "\n") {
			line := strings.TrimSpace(rawLine)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			matches := timelineLineRE.FindStringSubmatch(line)
			if len(matches) == 0 {
				return nil, fmt.Errorf("%s line %d: bad format", fixture.Timeline, lineIdx+1)
			}
			startStr, endStr, cmd := matches[1], matches[2], matches[3]
			vals := strings.Split(strings.TrimSpace(matches[4]), ",")
			for i := range vals {
				vals[i] = strings.TrimSpace(vals[i])
			}

			// Expand colour keyword into channel values if a single token is provided.
			if len(vals) == 1 {
				if cv, ok := colorMap[strings.ToLower(vals[0])]; ok {
					vals = make([]string, len(cv))
					for i, c := range cv {
						vals[i] = strconv.Itoa(c)
					}
				}
			}

			switch cmd {
			case "set":
				valueMap := map[string]int{}
				for i, attr := range orderedAttrs {
					if i >= len(vals) {
						break
					}
					v, _ := strconv.Atoi(vals[i])
					valueMap[attr] = v
				}
				events = append(events, Event{At: startStr, Set: &SetEvent{Fixture: fixture.ID, Values: valueMap}})

			case "fade":
				// Expand colour names for fade targets (single token → tuple)
				if len(vals) == 1 {
					if cv, ok := colorMap[strings.ToLower(vals[0])]; ok {
						vals = make([]string, len(cv))
						for i, c := range cv {
							vals[i] = strconv.Itoa(c)
						}
					}
				}
				toValues := map[string]int{}
				for i, attr := range orderedAttrs {
					if i >= len(vals) {
						break
					}
					v, _ := strconv.Atoi(vals[i])
					toValues[attr] = v
				}
				startMS, _ := parseTimestampMS(startStr)
				endMS, _ := parseTimestampMS(endStr)
				events = append(events, Event{At: startStr, Fade: &FadeEvent{Fixture: fixture.ID, ToMap: toValues, Dur: fmt.Sprintf("%dms", endMS-startMS)}})

			case "temp_set":
				valueMap := map[string]int{}
				for i, attr := range orderedAttrs {
					if i >= len(vals) {
						break
					}
					v, _ := strconv.Atoi(vals[i])
					valueMap[attr] = v
				}
				startMS, _ := parseTimestampMS(startStr)
				endMS, _ := parseTimestampMS(endStr)
				dur := fmt.Sprintf("%dms", endMS-startMS)
				events = append(events, Event{At: startStr, TempSet: &TempSetEvent{Fixture: fixture.ID, ToMap: valueMap, Dur: dur}})

			case "temp_fade":
				valueMap := map[string]int{}
				for i, attr := range orderedAttrs {
					if i >= len(vals)-3 {
						break
					}
					v, _ := strconv.Atoi(vals[i])
					valueMap[attr] = v
				}
				if len(vals) < len(orderedAttrs)+3 {
					return nil, fmt.Errorf("%s line %d: temp_fade requires fade_in, hold, fade_out", fixture.Timeline, lineIdx+1)
				}
				fadeInStr, holdStr, fadeOutStr := vals[len(vals)-3], vals[len(vals)-2], vals[len(vals)-1]
				events = append(events, Event{At: startStr, TempFade: &TempFadeEvent{Fixture: fixture.ID, ToMap: valueMap, FadeIn: fadeInStr, Hold: holdStr, FadeOut: fadeOutStr}})

			default:
				return nil, fmt.Errorf("%s line %d: unknown cmd %q", fixture.Timeline, lineIdx+1, cmd)
			}
		}
	}
	return events, nil
}

// CompiledEvent binds a parsed Event to its absolute start time in milliseconds.
type CompiledEvent struct {
	TimeMS int64
	Event  Event
}

func compileFromEvents(proj *Project, events []Event) ([]CompiledEvent, error) {
	var compiled []CompiledEvent
	for _, e := range events {
		t, err := parseTimestampMS(e.At)
		if err != nil {
			return nil, fmt.Errorf("bad event time %q: %w", e.At, err)
		}
		compiled = append(compiled, CompiledEvent{TimeMS: t, Event: e})
	}
	sort.Slice(compiled, func(i, j int) bool { return compiled[i].TimeMS < compiled[j].TimeMS })
	return compiled, nil
}

// run plays audio and executes compiled events, emitting DMX frames over Art‑Net.
func run(proj *Project, compiled []CompiledEvent) error {
	audioStream, format, err := decodeAudio(proj.Audio)
	if err != nil {
		return err
	}
	defer audioStream.Close()

	must(speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10)))

	sender, err := NewArtNetSender(proj.BroadcastSubnet)
	if err != nil {
		return fmt.Errorf("failed to set up Art-Net sender: %v", err)
	}

	fmt.Printf("Playing %s, broadcasting Art-Net frames...\n", proj.Audio)

	dmxFrame := make([]byte, 512)

	// Start the frame loop goroutine (sends DMX + ArtSync at 40 Hz)
	go runFrameLoop(sender, dmxFrame, 0)

	// Signal when audio completes.
	audioDone := make(chan bool)
	speaker.Play(beep.Seq(audioStream, beep.Callback(func() {
		fmt.Println("Audio complete.")
		close(audioDone)
	})))

	startTime := time.Now().Add(time.Duration(proj.OffsetMS) * time.Millisecond)
	eventIndex := 0

	for eventIndex < len(compiled) {
		elapsedMS := time.Since(startTime).Milliseconds()
		if elapsedMS < compiled[eventIndex].TimeMS {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		ev := compiled[eventIndex].Event
		switch {
		case ev.Set != nil:
			for attr, val := range ev.Set.Values {
				ch := proj.Profiles[proj.findProfile(ev.Set.Fixture)].Channels[attr]
				base := proj.findBase(ev.Set.Fixture)
				dmxFrame[base+ch-2] = byte(clamp(val, 0, 255))
			}

		case ev.Fade != nil:
			dur, err := time.ParseDuration(ev.Fade.Dur)
			if err != nil {
				fmt.Println("Invalid fade duration:", ev.Fade.Dur)
				break
			}
			if dur <= 0 {
				fmt.Println("Skipping fade with zero duration")
				break
			}

			profile := proj.Profiles[proj.findProfile(ev.Fade.Fixture)]
			base := proj.findBase(ev.Fade.Fixture)

			// Ensure we have target values
			to := ev.Fade.ToMap
			if len(to) == 0 {
				fmt.Println("Skipping fade with empty ToMap for", ev.Fade.Fixture)
				break
			}

			// Snapshot start values for this fixture
			from := make(map[string]int)
			for attr, ch := range profile.Channels {
				from[attr] = int(dmxFrame[base+ch-2])
			}

			// Smooth fade at 5ms resolution in a goroutine to avoid blocking the main loop.
			go func(from, to map[string]int, base int, profile Profile, dur time.Duration) {
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				t0 := time.Now()
				for range ticker.C {
					progress := float64(time.Since(t0)) / float64(dur)
					if progress > 1 {
						progress = 1
					}
					for attr, ch := range profile.Channels {
						startVal, ok1 := from[attr]
						endVal, ok2 := to[attr]
						if !ok1 || !ok2 {
							continue
						}
						cur := int(float64(startVal) + float64(endVal-startVal)*progress)
						dmxFrame[base+ch-2] = byte(clamp(cur, 0, 255))
					}
					if progress >= 1 {
						break
					}
				}
			}(from, to, base, profile, dur)

		case ev.TempSet != nil:
			profile := proj.Profiles[proj.findProfile(ev.TempSet.Fixture)]
			base := proj.findBase(ev.TempSet.Fixture)

			// Snapshot only this fixture's channel range
			snapshot := make([]byte, len(profile.Channels))
			for _, ch := range profile.Channels {
				snapshot[ch-1] = dmxFrame[base+ch-2]
			}

			// Apply temporary values
			for attr, val := range ev.TempSet.ToMap {
				ch := profile.Channels[attr]
				dmxFrame[base+ch-2] = byte(clamp(val, 0, 255))
			}

			// Restore after duration
			dur, _ := time.ParseDuration(ev.TempSet.Dur)
			go func(base int, profile Profile, snapshot []byte, dur time.Duration) {
				time.Sleep(dur)
				for _, ch := range profile.Channels {
					dmxFrame[base+ch-2] = snapshot[ch-1]
				}
			}(base, profile, snapshot, dur)

		case ev.TempFade != nil:
			fadeInDur, _ := time.ParseDuration(ev.TempFade.FadeIn)
			holdDur, _ := time.ParseDuration(ev.TempFade.Hold)
			fadeOutDur, _ := time.ParseDuration(ev.TempFade.FadeOut)

			go func(fx TempFadeEvent) {
				for attr, val := range fx.ToMap {
					ch := proj.Profiles[proj.findProfile(fx.Fixture)].Channels[attr]
					base := proj.findBase(fx.Fixture)

					// Fade in
					for v := 0; v <= val; v++ {
						dmxFrame[base+ch-2] = byte(clamp(v, 0, 255))
						time.Sleep(fadeInDur / time.Duration(max(1, val)))
					}
					time.Sleep(holdDur)

					// Fade out
					for v := val; v >= 0; v-- {
						dmxFrame[base+ch-2] = byte(clamp(v, 0, 255))
						time.Sleep(fadeOutDur / time.Duration(max(1, val)))
					}
				}
			}(*ev.TempFade)
		}

		eventIndex++
	}

	<-audioDone
	return nil
}

func (p *Project) findProfile(fxID string) string {
	for _, fx := range p.Patch {
		if fx.ID == fxID {
			return fx.Profile
		}
	}
	return ""
}

func (p *Project) findBase(fxID string) int {
	for _, fx := range p.Patch {
		if fx.ID == fxID {
			return fx.Base
		}
	}
	return 1
}

// max returns the larger of a and b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// main loads a YAML show file, compiles timelines into events, and runs the show.
func main() {
	poll := flag.Bool("poll", false, "Poll for Art-Net nodes and exit")
	flag.Parse()

	if *poll {
		pollArtNetNodes()
		os.Exit(0)
	}

	if flag.NArg() < 1 {
		fmt.Println("Usage: dmx_go [flags] show.yaml")
		os.Exit(1)
	}

	cfgPath := flag.Arg(0)
	f, err := os.ReadFile(cfgPath)
	must(err)

	var proj Project
	must(yaml.Unmarshal(f, &proj))

	events, err := loadTimelines(&proj)
	must(err)

	compiled, err := compileFromEvents(&proj, events)
	must(err)

	must(run(&proj, compiled))
	fmt.Println("Done.")
}
