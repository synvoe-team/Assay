// Package registry 稳定性检测项注册表：新增一类 = 在 all 里追加一项，不动框架。
// 与质量 registry 刻意分离 —— 稳定性 probe 无评分检查点，启动校验只认 ID 唯一/非空。
package registry

import (
	"fmt"

	"github.com/Yukiho0287/assay/server/internal/probe/stability"
)

// all 注册序即展示序：请求量轻的排前面（阶梯并发 < RPM < TPM）。
var all = []stability.Probe{
	stability.NewConcurrencyLadder(),
	stability.NewRpmProbe(),
	stability.NewTpmProbe(),
}

// init fail-fast：ID 缺失/重复在进程启动时暴露。稳定性不校验 Checkpoints（产出指标非 pass/fail）。
func init() {
	seen := map[string]bool{}
	for _, p := range all {
		if p.Info.ID == "" {
			panic("稳定性检测项 ID 不能为空")
		}
		if seen[p.Info.ID] {
			panic(fmt.Sprintf("稳定性检测项 ID %q 重复", p.Info.ID))
		}
		seen[p.Info.ID] = true
		if p.Run == nil {
			panic(fmt.Sprintf("稳定性检测项 %q 未提供 Run", p.Info.ID))
		}
	}
}

// All 返回全部稳定性检测项（注册序即展示序）。
func All() []stability.Probe { return all }

// Get 按 ID 查稳定性检测项。
func Get(id string) (stability.Probe, bool) {
	for _, p := range all {
		if p.Info.ID == id {
			return p, true
		}
	}
	return stability.Probe{}, false
}
