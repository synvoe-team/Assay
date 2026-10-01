package stability

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	ladderProbeID = "concurrency_ladder"
	// ladderPrompt 固定小 prompt：稳定输入、有一定生成量，供延迟观测
	ladderPrompt = "用一句话简要介绍你自己。"
)

// NewConcurrencyLadder 阶梯并发检测项（闭环）：逐档固定并发压测，测延迟曲线 + 吞吐 + 错误率。
func NewConcurrencyLadder() Probe {
	return Probe{
		Info: Info{
			ID:          ladderProbeID,
			Name:        "阶梯并发",
			Description: "按并发阶梯逐档固定并发发压，每档统计 TTFB/TTFT/总耗时分位数、吞吐与错误分类，观察延迟随并发上升的拐点。闭环模型，不做速率发现。",
			Protocols:   nil, // 三协议通用
			EstRequests: estLadderRequests,
		},
		Run: runLadder,
	}
}

// estLadderRequests 最坏预估：各档（计入 + 预热）之和。
func estLadderRequests(p StabilityParams) int {
	per := p.RequestsPerStage + p.WarmupPerStage
	return len(p.ConcurrencyLadder) * per
}

func runLadder(ctx context.Context, in RunInput) error {
	total := estLadderRequests(in.Params)
	var (
		mu   sync.Mutex
		done int
	)
	var overall []Sample // 各档剔除预热后的样本，供 __overall__ 汇总

	for si, c := range in.Params.ConcurrencyLadder {
		stage := fmt.Sprintf("c%d", c)
		samples, stop, err := runLadderStage(ctx, in, si, stage, c, &mu, &done, total)
		if err != nil {
			return err
		}
		sm := evaluateStage(in.Probe, stage, si, c, samples)
		if in.Metric != nil {
			if err := in.Metric(ctx, sm); err != nil {
				return err
			}
		}
		overall = append(overall, measured(samples)...)
		if stop {
			break // 硬闸触发，后续档不再跑（任务仍照常 succeeded）
		}
	}

	om := evaluateOverall(in.Probe, overall)
	if in.Metric != nil {
		if err := in.Metric(ctx, om); err != nil {
			return err
		}
	}
	return nil
}

// runLadderStage 跑一档：先 WarmupPerStage 预热、再 RequestsPerStage 计入，均固定并发 c。
// 返回本档全部样本（按 seq 升序）、是否因硬闸提前收敛、致命错误。
func runLadderStage(ctx context.Context, in RunInput, stageIndex int, stage string, c int, mu *sync.Mutex, done *int, total int) ([]Sample, bool, error) {
	n := in.Params.WarmupPerStage + in.Params.RequestsPerStage
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c)

	var smu sync.Mutex
	samples := make([]Sample, 0, n)
	stop := false

	for seq := 0; seq < n; seq++ {
		// 硬闸检查在派发前（主 goroutine）：碰上限即停派后续，本档收敛
		if !in.Caps.Reserve() {
			stop = true
			break
		}
		seq := seq
		warmup := seq < in.Params.WarmupPerStage
		g.Go(func() error {
			if gctx.Err() != nil {
				return gctx.Err()
			}
			// 闭环：dispatched_at 记实际起飞时刻（并发槽就绪即发）
			dispatchedAt := time.Now()
			o := doRequest(gctx, in.Client, in.Codec, in.Target.BaseURL, in.APIKey,
				in.Target.Model, uniquePrompt(in.Nonce, stage, seq, ladderPrompt), in.Params.LadderMaxTokens, in.Params.RequestTimeoutMs)
			// 中止（关停/取消）时不落污染样本：context canceled 非渠道行为
			if gctx.Err() != nil {
				return gctx.Err()
			}
			if in.Caps != nil && o.Usage.Ok {
				in.Caps.AddTokens(o.Usage.Prompt + o.Usage.Completion)
			}
			s := sampleFrom(stage, stageIndex, seq, warmup, dispatchedAt, in.Codec.ID(), o)

			smu.Lock()
			samples = append(samples, s)
			smu.Unlock()

			if in.Sample != nil {
				if err := in.Sample(gctx, s); err != nil {
					return fmt.Errorf("样本落库失败: %w", err)
				}
			}
			// 进度回调在锁内，保证 done 递增序落库（并发完成时旧值不覆盖新值）
			mu.Lock()
			*done++
			if in.Progress != nil {
				in.Progress(gctx, *done, total)
			}
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, false, err
	}
	// 并发 append 顺序不定，按 seq 排回稳定序（图表/导出可复现）
	sort.Slice(samples, func(i, j int) bool { return samples[i].Seq < samples[j].Seq })
	return samples, stop, nil
}
