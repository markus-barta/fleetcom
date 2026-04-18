package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// HwStatic mirrors db.HwStatic on the server — stable hardware profile.
type HwStatic struct {
	CPUModel      string    `json:"cpu_model,omitempty"`
	CPUCores      int       `json:"cpu_cores,omitempty"`
	MemTotalBytes uint64    `json:"mem_total_bytes,omitempty"`
	OSPretty      string    `json:"os_pretty,omitempty"`
	KernelVersion string    `json:"kernel_version,omitempty"`
	Mounts        []HwMount `json:"mounts,omitempty"`
}

type HwMount struct {
	Mountpoint string `json:"mountpoint"`
	Fstype     string `json:"fstype"`
	Device     string `json:"device,omitempty"`
}

// HwLive mirrors db.HwLive on the server.
type HwLive struct {
	CPULoad1      float64  `json:"cpu_load_1"`
	CPULoad5      float64  `json:"cpu_load_5"`
	CPULoad15     float64  `json:"cpu_load_15"`
	CPUUsedPct    float64  `json:"cpu_used_pct,omitempty"`
	MemTotalBytes uint64   `json:"mem_total_bytes,omitempty"`
	MemUsedBytes  uint64   `json:"mem_used_bytes,omitempty"`
	MemUsedPct    float64  `json:"mem_used_pct,omitempty"`
	CPUTempC      *float64 `json:"cpu_temp_c,omitempty"`
	GPUTempC      *float64 `json:"gpu_temp_c,omitempty"`
}

// collectStatic builds the stable hardware profile. Every field is
// best-effort — missing /proc/cpuinfo (e.g., non-Linux host) yields an
// empty CPUModel, not an error.
func collectStatic() HwStatic {
	out := HwStatic{
		OSPretty:      getOS(),
		KernelVersion: getKernel(),
	}
	if model, cores := readCPUInfo(); model != "" {
		out.CPUModel = model
		out.CPUCores = cores
	}
	if total := readMemTotal(); total > 0 {
		out.MemTotalBytes = total
	}
	out.Mounts = readMounts()
	return out
}

// collectLive is the per-heartbeat snapshot. cpuCores is the cached core
// count from the most recent static scan — passed in so the UI can render
// CPU % without needing a second request.
func collectLive(cpuCores int) HwLive {
	l1, l5, l15 := readLoadAvg()
	out := HwLive{
		CPULoad1:  l1,
		CPULoad5:  l5,
		CPULoad15: l15,
	}
	if cpuCores > 0 {
		out.CPUUsedPct = 100.0 * l1 / float64(cpuCores)
	}
	if total, avail := readMemTotalAvail(); total > 0 {
		out.MemTotalBytes = total
		out.MemUsedBytes = total - avail
		if total > 0 {
			out.MemUsedPct = 100.0 * float64(total-avail) / float64(total)
		}
	}
	if t := readCPUTemp(); t != nil {
		out.CPUTempC = t
	}
	if t := readGPUTemp(); t != nil {
		out.GPUTempC = t
	}
	return out
}

// hwStaticHash returns a stable hash of a HwStatic value so bosun can
// detect changes and only send Static when it actually moved.
func hwStaticHash(s HwStatic) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---------- readers ----------

func readCPUInfo() (string, int) {
	for _, path := range []string{"/host/proc/cpuinfo", "/proc/cpuinfo"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		model := ""
		cores := 0
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if model == "" && strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					model = strings.TrimSpace(line[i+1:])
				}
			}
			if strings.HasPrefix(line, "processor") {
				cores++
			}
		}
		return model, cores
	}
	return "", 0
}

// readMemTotal returns MemTotal in bytes.
func readMemTotal() uint64 {
	total, _ := readMemTotalAvail()
	return total
}

// readMemTotalAvail returns (MemTotal, MemAvailable) in bytes.
func readMemTotalAvail() (uint64, uint64) {
	for _, path := range []string{"/host/proc/meminfo", "/proc/meminfo"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		var total, avail uint64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "MemTotal:") {
				total = parseMeminfoKB(line) * 1024
			} else if strings.HasPrefix(line, "MemAvailable:") {
				avail = parseMeminfoKB(line) * 1024
			}
			if total > 0 && avail > 0 {
				break
			}
		}
		f.Close()
		return total, avail
	}
	return 0, 0
}

func parseMeminfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	n, _ := strconv.ParseUint(fields[1], 10, 64)
	return n
}

func readLoadAvg() (float64, float64, float64) {
	for _, path := range []string{"/host/proc/loadavg", "/proc/loadavg"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) >= 3 {
			l1, _ := strconv.ParseFloat(fields[0], 64)
			l5, _ := strconv.ParseFloat(fields[1], 64)
			l15, _ := strconv.ParseFloat(fields[2], 64)
			return l1, l5, l15
		}
	}
	return 0, 0, 0
}

// readMounts parses /etc/mtab (or /proc/mounts as fallback). Filters out
// virtual/pseudo filesystems that aren't useful to show in the UI.
func readMounts() []HwMount {
	for _, path := range []string{"/host/etc/mtab", "/host/proc/mounts", "/proc/mounts"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		out := []HwMount{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 3 {
				continue
			}
			device, mountpoint, fstype := fields[0], fields[1], fields[2]
			if isPseudoFS(fstype) {
				continue
			}
			out = append(out, HwMount{Mountpoint: mountpoint, Fstype: fstype, Device: device})
		}
		f.Close()
		return out
	}
	return nil
}

// isPseudoFS hides kernel/virtual filesystems from the mount table so the
// UI only shows real storage. Kept close to what fastfetch / df default to
// skipping.
func isPseudoFS(fstype string) bool {
	switch fstype {
	case "proc", "sysfs", "devpts", "devtmpfs", "tmpfs", "cgroup", "cgroup2",
		"pstore", "bpf", "tracefs", "debugfs", "securityfs", "hugetlbfs",
		"mqueue", "autofs", "configfs", "fusectl", "binfmt_misc", "rpc_pipefs",
		"nsfs", "ramfs", "squashfs", "overlay":
		return true
	}
	return false
}

// readCPUTemp returns the hottest Package id / CPU hwmon sensor, in Celsius,
// or nil if nothing is reported. Covers the common cases without trying to
// be exhaustive: look for hwmon devices named "coretemp" / "k10temp" /
// "cpu_thermal", take the max temp*_input across them.
func readCPUTemp() *float64 {
	candidates := []string{"coretemp", "k10temp", "zenpower", "cpu_thermal", "acpitz"}
	return maxHwmonTempByName(candidates)
}

// readGPUTemp looks for common GPU hwmon names (amdgpu, nouveau, radeon).
// NVIDIA proprietary drivers don't expose hwmon entries, so those stay nil.
func readGPUTemp() *float64 {
	candidates := []string{"amdgpu", "nouveau", "radeon"}
	return maxHwmonTempByName(candidates)
}

func maxHwmonTempByName(names []string) *float64 {
	base := "/host/sys/class/hwmon"
	if _, err := os.Stat(base); err != nil {
		base = "/sys/class/hwmon"
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var best float64
	found := false
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		nb, err := os.ReadFile(filepath.Join(dir, "name"))
		if err != nil {
			continue
		}
		n := strings.TrimSpace(string(nb))
		match := false
		for _, want := range names {
			if n == want {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		// Scan temp*_input files. Values are milli-degrees Celsius.
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasPrefix(f.Name(), "temp") || !strings.HasSuffix(f.Name(), "_input") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			mdeg, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			if err != nil {
				continue
			}
			c := float64(mdeg) / 1000.0
			if !found || c > best {
				best = c
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	return &best
}
