package stability

import (
	"context"
	"reflect"
	"testing"
)

// fakeStages 按速率给出预设结局的假档；记录实际跑过的速率序列
type fakeStages struct {
	limitedAbove float64          // 速率 > 此值即判限速
	stopAt       map[float64]bool // 跑到这些速率时撞硬闸
	ran          []float64
}

func (f *fakeStages) run(rate float64) (stageOutcome, error) {
	f.ran = append(f.ran, rate)
	return stageOutcome{limited: rate > f.limitedAbove, stopped: f.stopAt[rate]}, nil
}

func TestSearchBoundaryConverges(t *testing.T) {
	f := &fakeStages{limitedAbove: 5}
	b, err := searchBoundary(context.Background(), rampRates(2, 20), 4, 0.5, f.run)
	if err != nil {
		t.Fatal(err)
	}
	// ramp 2/4 通过、8 限速；二分 6(限)→5(过)→5.5(限)；5.5-5=0.5 不大于容差即停
	if want := []float64{2, 4, 8, 6, 5, 5.5}; !reflect.DeepEqual(f.ran, want) {
		t.Errorf("跑过的速率 %v，期望 %v", f.ran, want)
	}
	if b.lo != 5 || b.hi != 5.5 || !b.haveHi || b.truncated {
		t.Errorf("结论 %+v，期望 lo=5 hi=5.5 haveHi 未截断", b)
	}
}

func TestSearchBoundaryNeverLimited(t *testing.T) {
	f := &fakeStages{limitedAbove: 1e9}
	b, _ := searchBoundary(context.Background(), rampRates(4, 16), 4, 0.5, f.run)
	if b.lo != 16 || b.haveHi || b.truncated {
		t.Errorf("一路不限速应升到护栏顶，得 %+v", b)
	}
}

// 修复的 bug：升压途中撞硬闸、还没见过限速 → 截断，lo 只是下界；撞闸那档的「通过」不可信，不得抬高 lo
func TestSearchBoundaryTruncatedInRamp(t *testing.T) {
	f := &fakeStages{limitedAbove: 1e9, stopAt: map[float64]bool{8: true}}
	b, _ := searchBoundary(context.Background(), rampRates(2, 20), 4, 0.5, f.run)
	if !b.truncated || b.haveHi || b.lo != 4 {
		t.Errorf("结论 %+v，期望 truncated、无 hi、lo=4（撞闸档 8 不计为通过）", b)
	}
	if want := []float64{2, 4, 8}; !reflect.DeepEqual(f.ran, want) {
		t.Errorf("撞闸后不应再跑，实际 %v", f.ran)
	}
}

// 撞闸那档若已判限速，限速证据仍有效：记 hi，同时标截断
func TestSearchBoundaryLimitedAndStopped(t *testing.T) {
	f := &fakeStages{limitedAbove: 5, stopAt: map[float64]bool{8: true}}
	b, _ := searchBoundary(context.Background(), rampRates(2, 20), 4, 0.5, f.run)
	if !b.truncated || !b.haveHi || b.hi != 8 || b.lo != 4 {
		t.Errorf("结论 %+v，期望 truncated、hi=8、lo=4", b)
	}
}

// 二分途中撞闸：撞闸档的「通过」不抬高 lo
func TestSearchBoundaryTruncatedInBinary(t *testing.T) {
	f := &fakeStages{limitedAbove: 5, stopAt: map[float64]bool{5: true}}
	b, _ := searchBoundary(context.Background(), rampRates(2, 20), 4, 0.5, f.run)
	// ramp 2/4/8(限) → 二分 6(限) → 5(过但撞闸) → 停
	if !b.truncated || b.lo != 4 || b.hi != 6 {
		t.Errorf("结论 %+v，期望 truncated lo=4 hi=6", b)
	}
}
