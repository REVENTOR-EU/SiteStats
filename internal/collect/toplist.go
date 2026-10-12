package collect

import "sort"

// topList keeps a bounded top-N list of processes by CPU.
type topList []ProcInfo

func (t *topList) add(p ProcInfo) {
	*t = append(*t, p)
}

func (t topList) sorted(n int) []ProcInfo {
	sort.Slice(t, func(i, j int) bool { return t[i].CPU > t[j].CPU })
	if len(t) > n {
		t = t[:n]
	}
	out := make([]ProcInfo, len(t))
	copy(out, t)
	return out
}
