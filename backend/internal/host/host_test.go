package host

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// procStat is trimmed from a real /proc/stat on a 4-core machine.
const procStat = `cpu  10132153 290696 3084719 46828483 16683 0 25195 0 175628 0
cpu0 1393280 32966 572056 13343292 6130 0 17875 0 0 0
cpu1 1335236 31578 568744 13372316 4086 0 3617 0 0 0
cpu2 3793346 113022 951548 10044216 3360 0 2059 0 175628 0
cpu3 3610291 113130 992371 10068659 3107 0 1644 0 0 0
intr 1462898 139 0 0 0 0
ctxt 12547729
btime 1727600000
processes 26442
procs_running 2
procs_blocked 0
`

// procMeminfo is trimmed from a real /proc/meminfo on a 16 GB machine.
const procMeminfo = `MemTotal:       15734656 kB
MemFree:          812344 kB
MemAvailable:    9421020 kB
Buffers:          310228 kB
Cached:          7969232 kB
SwapCached:            0 kB
Active:          9123232 kB
Inactive:        4566660 kB
SwapTotal:       4194300 kB
SwapFree:        4194300 kB
`

func TestParseCPU(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    CPUTimes
		wantErr string
	}{
		{
			name:  "real /proc/stat",
			input: procStat,
			// total = user+nice+system+idle+iowait+irq+softirq+steal (guest
			// columns excluded); idle = idle+iowait.
			want: CPUTimes{
				Total: 10132153 + 290696 + 3084719 + 46828483 + 16683 + 0 + 25195 + 0,
				Busy:  10132153 + 290696 + 3084719 + 0 + 25195 + 0,
			},
		},
		{
			name:  "old kernel with only four columns",
			input: "cpu  100 0 50 850\n",
			want:  CPUTimes{Total: 1000, Busy: 150},
		},
		{name: "no aggregate line", input: "cpu0 1 2 3 4\nintr 5\n", wantErr: "no aggregate cpu line"},
		{name: "too few fields", input: "cpu  1 2 3\n", wantErr: "fields"},
		{name: "not a number", input: "cpu  1 x 3 4\n", wantErr: "cpu field 2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCPU(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCPU: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseCPU = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCPUPercent(t *testing.T) {
	prev := CPUTimes{Busy: 1000, Total: 10_000}

	tests := []struct {
		name   string
		cur    CPUTimes
		want   float64
		wantOK bool
	}{
		{name: "quarter busy", cur: CPUTimes{Busy: 1100, Total: 10_400}, want: 25, wantOK: true},
		{name: "fully busy", cur: CPUTimes{Busy: 1400, Total: 10_400}, want: 100, wantOK: true},
		{name: "idle", cur: CPUTimes{Busy: 1000, Total: 10_400}, want: 0, wantOK: true},
		{name: "same tick", cur: prev},
		{name: "counters went backwards", cur: CPUTimes{Busy: 10, Total: 100}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CPUPercent(prev, tt.cur)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("CPUPercent = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMemInfo(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Memory
		wantErr string
	}{
		{
			name:  "real /proc/meminfo",
			input: procMeminfo,
			want:  Memory{Total: 15734656 * 1024, Available: 9421020 * 1024},
		},
		{name: "no MemAvailable", input: "MemTotal: 100 kB\nMemFree: 50 kB\n", wantErr: "missing"},
		{name: "unexpected unit", input: "MemTotal: 100 MB\nMemAvailable: 50 kB\n", wantErr: "unexpected format"},
		{name: "not a number", input: "MemTotal: lots kB\nMemAvailable: 50 kB\n", wantErr: "MemTotal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseMemInfo(strings.NewReader(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMemInfo: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseMemInfo = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMemoryUsed(t *testing.T) {
	if got := (Memory{Total: 1000, Available: 300}).Used(); got != 700 {
		t.Errorf("Used = %d, want 700", got)
	}
	if got := (Memory{Total: 1000, Available: 5000}).Used(); got != 0 {
		t.Errorf("Used with Available > Total = %d, want 0", got)
	}
}

func TestReaderRead(t *testing.T) {
	proc := t.TempDir()
	for name, content := range map[string]string{"stat": procStat, "meminfo": procMeminfo} {
		if err := os.WriteFile(filepath.Join(proc, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(proc, t.TempDir())

	got, err := r.Read()

	if runtime.GOOS != "linux" {
		// The proc files parsed; only the Linux-only disk part is missing.
		if err == nil || !strings.Contains(err.Error(), "disk usage") {
			t.Fatalf("error = %v, want a disk usage error off Linux", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Memory.Total != 15734656*1024 || got.CPU.Total == 0 {
		t.Errorf("Read = %+v, want the fixture's CPU and memory", got)
	}
	d := got.Disk
	if d.Total == 0 || d.Used+d.Available > d.Total {
		t.Errorf("Disk = %+v, want Total > 0 and Used+Available <= Total", d)
	}
}

func TestReaderReadMissingFile(t *testing.T) {
	r := NewReader(t.TempDir(), t.TempDir()) // empty proc dir

	if _, err := r.Read(); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Errorf("error = %v, want one naming the missing stat file", err)
	}
}
