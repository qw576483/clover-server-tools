package load

import (
	"math/rand"
	"sort"
)

// Dist 延迟分布（单位：毫秒）。
//
// 用「蓄水池抽样」而不是全量保留：压测在高频长跑下样本可达百万级，全量存会把内存
// 打爆（1000 机器人 × 60s × 64 帧/s ≈ 384 万样本）。蓄水池在固定内存下仍能得到
// 无偏的分位数估计。
//
// 分位数来自抽样（估计值），而 count / min / max / avg 是精确值 ——
// 报告里只有分位数需要按"估计"来读。
//
// **零值可用**（limit<=0 时按 defaultDistLimit 走）：聚合用的 Dist 是直接声明出来的
// 零值，若在这里要求必须 NewDist 构造，漏掉一处就会静默退化成"只有 min/max、
// 分位数全 0"—— 这种 bug 在报告里几乎看不出来。
//
// 非并发安全：每个 Dist 由单个机器人的 goroutine 独占写入；
// 跨机器人汇总走 Merge，由 runner 统一加锁。
type Dist struct {
	samples []int64 // 蓄水池，长度 <= limit
	limit   int
	count   int64 // 见过的样本总数（含未被保留的）
	min     int64
	max     int64
	sum     int64 // 值已知的样本之和（含被替换出池的）
	sumN    int64 // 值已知的样本个数（= Add 调用次数）
}

// defaultDistLimit 单个机器人的蓄水池容量。
// 128 个样本足以给出稳定的 P90/P99，且 1000 个机器人的总内存只有约 1MB。
const defaultDistLimit = 128

// NewDist 创建分布，limit 为蓄水池容量（<=0 用 defaultDistLimit）。
func NewDist(limit int) *Dist {
	if limit <= 0 {
		limit = defaultDistLimit
	}
	return &Dist{limit: limit}
}

// Limit 返回（补默认后的）蓄水池容量。
func (d *Dist) Limit() int {
	if d.limit <= 0 {
		return defaultDistLimit
	}
	return d.limit
}

// Add 记入一个样本。
func (d *Dist) Add(ms int64) {
	if ms < 0 {
		ms = 0
	}
	if d.limit <= 0 {
		d.limit = defaultDistLimit // 零值兜底，见类型注释
	}

	d.count++
	d.sum += ms
	d.sumN++
	if d.count == 1 {
		d.min, d.max = ms, ms
	} else {
		if ms < d.min {
			d.min = ms
		}
		if ms > d.max {
			d.max = ms
		}
	}

	if len(d.samples) < d.limit {
		d.samples = append(d.samples, ms)
		return
	}
	// 蓄水池替换：以 limit/count 的概率替换池中随机一个位置。
	if j := rand.Int63n(d.count); j < int64(d.limit) {
		d.samples[j] = ms
	}
}

// Merge 并入另一份分布。
//
// 对方的样本逐个走一遍本池的蓄水池；对方未被保留的样本（超过它池容量的那部分）
// 值已不可知，只计入总数 —— count/avg 会因此略微失真，分位数不受影响。
// 压测报告看的是量级与分位趋势，这个近似足够，且实现一眼可懂。
func (d *Dist) Merge(o *Dist) {
	if o == nil {
		return
	}
	for _, v := range o.samples {
		d.Add(v)
	}
	if extra := o.count - int64(len(o.samples)); extra > 0 {
		d.count += extra
	}
}

// DistSnap 分布的快照（报告输出用）。
type DistSnap struct {
	Count int64   `json:"count"`
	Min   int64   `json:"min"`
	Max   int64   `json:"max"`
	Avg   float64 `json:"avg"`
	P50   int64   `json:"p50"`
	P90   int64   `json:"p90"`
	P99   int64   `json:"p99"`
}

// Snapshot 生成快照。空分布返回全零。
func (d *Dist) Snapshot() DistSnap {
	if d == nil || d.count == 0 {
		return DistSnap{}
	}
	s := DistSnap{Count: d.count, Min: d.min, Max: d.max}
	// avg 用 sum/sumN：sum 覆盖所有"值已知"的样本（含被替换出池的），
	// 比只对池内样本求平均更准确。
	if d.sumN > 0 {
		s.Avg = float64(d.sum) / float64(d.sumN)
	}
	if len(d.samples) > 0 {
		sorted := append([]int64(nil), d.samples...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		s.P50 = percentile(sorted, 50)
		s.P90 = percentile(sorted, 90)
		s.P99 = percentile(sorted, 99)
	}
	return s
}

// percentile 从已排序切片取 p 分位（最近秩法）。
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) - 1) * p / 100
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
