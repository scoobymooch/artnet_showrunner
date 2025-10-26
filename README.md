# ArtNet Showrunner

**ArtNet Showrunner** is a timeline-driven DMX + audio player built in Go.  
It synchronises lighting cues with audio playback and broadcasts DMX frames via Art-Net.

---

## ✨ Features

- Plays **MP3/WAV** audio with frame-accurate DMX sync  
- Supports **set**, **fade**, **temp_set**, and **temp_fade** timeline commands  
- Broadcasts **Art-Net DMX + ArtSync** packets at 40 Hz  
- Automatic colour keyword expansion (e.g. `red`, `cyan`, `warmwhite`)  
- YAML-based show configuration and per-fixture text timelines  
- Optional Art-Net node discovery (`--poll`)

---

## 📂 Project Structure

```
artnet_showrunner/
├── main.go
├── show.yaml
├── timelines/
│   ├── par1.txt
│   └── par2.txt
└── README.md
```

---

## ⚙️ Example `show.yaml`

```yaml
audio: MonsterMash.mp3
offset_ms: 0
profiles:
  par:
    channels:
      dimmer: 1
      red: 2
      green: 3
      blue: 4
patch:
  - id: par1
    profile: par
    base: 1
    timeline: timelines/par1.txt
  - id: par2
    profile: par
    base: 6
    timeline: timelines/par2.txt
broadcast_subnet: 192.168.1.255
```

---

## ⏱️ Example Timeline File (`timelines/par1.txt`)

```
0       1.0   set [red]
1.0     3.0   fade [blue]
3.0     5.0   temp_set [green]
5.0     10.0  temp_fade [white, 500ms, 1s, 500ms]
```

---

## 🚀 Usage

```bash
go run main.go show.yaml
```

Or to discover Art-Net nodes on your network:

```bash
go run main.go --poll
```

---

## 🧰 Requirements
- Go 1.21+
- Local Art-Net receiver or DMX node on the same subnet  

---

## 📜 License
MIT License © 2025 Matt Barr
