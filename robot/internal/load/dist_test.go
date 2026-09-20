package load

import "testing"

// TestDistZeroValue 守住一个曾经真出过的 bug：
// 聚合用的 Dist 是直接声明出来的零值，若 Add 不兜底 limit，样本一个都进不了池，
// 报告会变成「min/max 有值、avg 与所有分位数全 0」—— 在报告里几乎看不出来。
func TestDistZeroValue(t *testing.T) {
	var d Dist // 零值，不经过 NewDist
	d.Add(10)
	d.Add(20)
	d.Add(30)

	s := d.Snapshot()
	if s.Count != 3 {
		t.Fatalf("count = %d, want 3", s.Count)
	}
	if s.Min != 10 || s.Max != 30 {
		t.Fatalf("min/max = %d/%d, want 10/30", s.Min, s.Max)
	}
	if s.P50 == 0 || s.P99 == 0 {
		t.Fatalf("分位数全为 0（样本未进池）: %+v", s)
	}
	if s.Avg != 20 {
		t.Fatalf("avg = %v, want 20", s.Avg)
	}
}

func TestDistPercentile(t *testing.T) {
	d := NewDist(1000)
	for i := 1; i <= 100; i++ {
		d.Add(int64(i))
	}
	s := d.Snapshot()
	if s.Count != 100 {
		t.Fatalf("count = %d, want 100", s.Count)
	}
	// 最近秩法：(n-1)*p/100
	if s.P50 != 50 {
		t.Fatalf("p50 = %d, want 50", s.P50)
	}
	if s.P90 != 90 {
		t.Fatalf("p90 = %d, want 90", s.P90)
	}
	if s.P99 != 99 {
		t.Fatalf("p99 = %d, want 99", s.P99)
	}
	if s.Avg != 50.5 {
		t.Fatalf("avg = %v, want 50.5", s.Avg)
	}
}

// TestDistReservoirBounded 池容量必须封顶，否则长跑会把内存吃光。
func TestDistReservoirBounded(t *testing.T) {
	d := NewDist(16)
	for i := 0; i < 10000; i++ {
		d.Add(int64(i))
	}
	if got := len(d.samples); got != 16 {
		t.Fatalf("池内样本 = %d, want 16（未封顶）", got)
	}
	if s := d.Snapshot(); s.Count != 10000 {
		t.Fatalf("count = %d, want 10000", s.Count)
	}
	// min/max 是精确值，不受抽样影响。
	if s := d.Snapshot(); s.Min != 0 || s.Max != 9999 {
		t.Fatalf("min/max = %d/%d, want 0/9999", s.Min, s.Max)
	}
}

func TestDistMerge(t *testing.T) {
	a := NewDist(1000)
	a.Add(10)
	a.Add(20)

	var b Dist // 零值也要能参与合并
	b.Add(30)
	b.Add(40)

	a.Merge(&b)

	s := a.Snapshot()
	if s.Count != 4 {
		t.Fatalf("合并后 count = %d, want 4", s.Count)
	}
	if s.Min != 10 || s.Max != 40 {
		t.Fatalf("合并后 min/max = %d/%d, want 10/40", s.Min, s.Max)
	}
	if s.Avg != 25 {
		t.Fatalf("合并后 avg = %v, want 25", s.Avg)
	}
}

// TestDistMergeWithOverflow 对方未被保留的样本要计入总数（否则报告里样本数对不上）。
func TestDistMergeWithOverflow(t *testing.T) {
	big := NewDist(4)
	for i := 1; i <= 100; i++ {
		big.Add(int64(i))
	}

	var agg Dist
	agg.Merge(big)

	if s := agg.Snapshot(); s.Count != 100 {
		t.Fatalf("count = %d, want 100", s.Count)
	}
}
