package stability

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// TestFillerDeterministic 填充体：同种子同字节、长度精确、前缀性质、纯 ASCII、像自然文本（单词+空格+标点）
func TestFillerDeterministic(t *testing.T) {
	a := fillerText("n0nce123|c1|3", 5000)
	b := fillerText("n0nce123|c1|3", 5000)
	if a != b {
		t.Fatal("同种子两次生成不一致")
	}
	if len(a) != 5000 {
		t.Fatalf("长度 = %d，期望 5000", len(a))
	}
	if short := fillerText("n0nce123|c1|3", 1234); short != a[:1234] {
		t.Error("短填充应是长填充的前缀（同一确定性流截断）")
	}
	if fillerText("n0nce123|c1|4", 5000) == a {
		t.Error("不同种子不应生成相同文本")
	}
	if fillerText("x", 0) != "" {
		t.Error("长度 0 应为空串")
	}
	run := 0
	for i := 0; i < len(a); i++ {
		c := a[i]
		if c >= utf8.RuneSelf || (c < ' ' && c != '\n') {
			t.Fatalf("第 %d 字节 %q 非可打印 ASCII", i, c)
		}
		if c == ' ' || c == '\n' {
			run = 0
			continue
		}
		if run++; run > 20 {
			t.Fatalf("出现超过 20 字符的无空白长串（像 base64 而不像单词）：%q", a[max(0, i-30):i+1])
		}
	}
	for _, want := range []string{" ", ". ", "\n\n", ","} {
		if !strings.Contains(a, want) {
			t.Errorf("填充文本应含 %q", want)
		}
	}
}

// TestInputTargetRamp 递增：按本档排定顺序从 min 匀速涨到 max，单调不降，越界序号钳在 max
func TestInputTargetRamp(t *testing.T) {
	in := InputSpec{Mode: InputRamp, Min: 1000, Max: 2000}
	const n = 11
	prev := 0
	for seq := 0; seq < n; seq++ {
		got := in.target("nonce", "c1", seq, n)
		if got < prev {
			t.Fatalf("seq=%d 目标 %d < 前一个 %d，非单调", seq, got, prev)
		}
		prev = got
	}
	if first, last := in.target("nonce", "c1", 0, n), in.target("nonce", "c1", n-1, n); first != 1000 || last != 2000 {
		t.Errorf("首/末 = %d/%d，期望 1000/2000", first, last)
	}
	if got := in.target("nonce", "c1", 5, n); got != 1500 {
		t.Errorf("中点 = %d，期望 1500", got)
	}
	if got := in.target("nonce", "c1", n+3, n); got != 2000 {
		t.Errorf("越界序号 = %d，期望钳在 2000", got)
	}
	if got := in.target("nonce", "c1", 0, 1); got != 1000 {
		t.Errorf("单请求档 = %d，期望 min", got)
	}
}

// TestInputTargetJitter 抖动：落在 [min,max]、同 nonce 可复现、换 nonce 换序列、不是常数
func TestInputTargetJitter(t *testing.T) {
	in := InputSpec{Mode: InputJitter, Min: 500, Max: 900}
	seen := map[int]bool{}
	diff := 0
	for seq := 0; seq < 200; seq++ {
		v := in.target("nonceAAA", "r2", seq, 200)
		if v < 500 || v > 900 {
			t.Fatalf("seq=%d 目标 %d 越出 [500,900]", seq, v)
		}
		if v != in.target("nonceAAA", "r2", seq, 200) {
			t.Fatal("同 nonce 同序号应复现同一目标")
		}
		if v != in.target("nonceBBB", "r2", seq, 200) {
			diff++
		}
		seen[v] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 次抖动只出现 %d 个不同值，分布过窄", len(seen))
	}
	if diff < 150 {
		t.Errorf("换 nonce 只有 %d/200 个目标不同，种子没生效", diff)
	}
}

func TestInputTargetFixedNone(t *testing.T) {
	if got := (InputSpec{Mode: InputFixed, Value: 8000}).target("n", "c1", 3, 10); got != 8000 {
		t.Errorf("fixed = %d", got)
	}
	if got := (InputSpec{Mode: InputNone}).target("n", "c1", 3, 10); got != 0 {
		t.Errorf("none = %d，期望 0（不塑形）", got)
	}
	cases := []struct {
		in   InputSpec
		mean float64
	}{
		{InputSpec{Mode: InputFixed, Value: 8000}, 8000},
		{InputSpec{Mode: InputRamp, Min: 1000, Max: 2000}, 1500},
		{InputSpec{Mode: InputJitter, Min: 1000, Max: 3000}, 2000},
		{InputSpec{Mode: InputNone}, 0},
	}
	for _, c := range cases {
		if got := c.in.mean(); got != c.mean {
			t.Errorf("%+v mean = %v，期望 %v", c.in, got, c.mean)
		}
	}
}

func TestNominalTokens(t *testing.T) {
	cases := map[string]int{"": 0, "abcd": 1, "abcde": 2, "用一句话": 4, "ab用": 2}
	for s, want := range cases {
		if got := nominalTokens(s); got != want {
			t.Errorf("nominalTokens(%q) = %d，期望 %d", s, got, want)
		}
	}
}

// TestSpecDefaultGolden 负载画像全缺省（h=0、不塑形、无输出目标）时 prompt 与上线前逐字节一致
func TestSpecDefaultGolden(t *testing.T) {
	in := RunInput{Nonce: "ab12cd34"}
	sp := in.spec("r2", 7, 0, rpmOutput)
	p := in.prompt(sp)
	if p.Shared != "" || p.Unique != "[ab12cd34 r2-7] 用一句话简要介绍你自己。" {
		t.Errorf("prompt = %+v，期望无共享前缀 + 原金标", p)
	}
	if sp.MaxTokens != DefaultRpmMaxTokens || sp.TargetInput != 0 || sp.TargetOutput != 0 {
		t.Errorf("spec = %+v，期望 max=16、无输入/输出目标", sp)
	}
	if sp := in.spec("c4", 0, 22, ladderOutput); sp.MaxTokens != DefaultLadderMaxTokens || sp.TargetOutput != 0 {
		t.Errorf("阶梯默认 spec = %+v，期望 max=2048 无输出目标", sp)
	}
	// TPM 默认就要写满砝码：顶格数数 prompt，目标输出 = 256
	sp = in.spec("t200", 1, 0, tpmOutput)
	if sp.MaxTokens != DefaultTpmMaxTokens || sp.TargetOutput != DefaultTpmMaxTokens {
		t.Errorf("TPM 默认 spec = %+v，期望 max=目标=256", sp)
	}
	if got := in.prompt(sp).Unique; got != "[ab12cd34 t200-1] "+countPrompt {
		t.Errorf("TPM 默认 prompt = %q", got)
	}
}

// TestSpecOutputTarget 设了输出目标：三个 probe 一律 max_tokens=N + 顶格数数 prompt
func TestSpecOutputTarget(t *testing.T) {
	in := RunInput{Nonce: "ab12cd34", Params: StabilityParams{Workload: Workload{Output: 128}}}
	for _, def := range []outputDefault{ladderOutput, rpmOutput, tpmOutput} {
		sp := in.spec("c1", 0, 1, def)
		if sp.MaxTokens != 128 || sp.TargetOutput != 128 {
			t.Errorf("spec = %+v，期望 max=目标=128", sp)
		}
		if !strings.HasSuffix(in.prompt(sp).Unique, countPrompt) {
			t.Errorf("有输出目标时问题应为顶格数数 prompt")
		}
	}
}

// TestSpecShapedInput 输入塑形：唯一标记打头（破缓存）、问题在最后一句、总长 ≈ 目标×字符比
func TestSpecShapedInput(t *testing.T) {
	w := Workload{Input: InputSpec{Mode: InputFixed, Value: 4000}}
	in := RunInput{Nonce: "ab12cd34", Params: StabilityParams{Workload: w}, Plan: buildPlan(w, "ab12cd34", 4)}
	sp := in.spec("c2", 5, 22, ladderOutput)
	if sp.TargetInput != 4000 {
		t.Fatalf("TargetInput = %d", sp.TargetInput)
	}
	p := in.prompt(sp)
	if p.Shared != "" {
		t.Error("h=0 不应有共享前缀")
	}
	if !strings.HasPrefix(p.Unique, "[ab12cd34 c2-5] ") {
		t.Errorf("唯一标记应在最前面破缓存，得 %q…", p.Unique[:40])
	}
	if !strings.HasSuffix(p.Unique, "\n\n"+paragraphQuestion) {
		t.Errorf("有填充、无输出目标时问题应是数段落（输出可控），结尾 %q", p.Unique[len(p.Unique)-40:])
	}
	if got := nominalTokens(p.Text()); math.Abs(float64(got-4000)) > 40 {
		t.Errorf("按名义比 4 估算的 prompt token = %d，期望 ≈4000（±1%%）", got)
	}
	if q := in.prompt(in.spec("c2", 6, 22, ladderOutput)); q.Unique == p.Unique {
		t.Error("不同请求的唯一段不应相同")
	}
}

// TestSpecSharedPrefix h>0：共享前缀字节相同且约 h×目标，唯一标记挪到共享前缀之后
func TestSpecSharedPrefix(t *testing.T) {
	w := Workload{Input: InputSpec{Mode: InputFixed, Value: 4000}, CacheHitRate: 0.5}
	plan := buildPlan(w, "ab12cd34", 4)
	if plan.SharedTokens != 2000 {
		t.Fatalf("SharedTokens = %d，期望 2000", plan.SharedTokens)
	}
	in := RunInput{Nonce: "ab12cd34", Params: StabilityParams{Workload: w}, Plan: plan}
	p1 := in.prompt(in.spec("c1", 0, 10, ladderOutput))
	p2 := in.prompt(in.spec("c4", 3, 10, ladderOutput))
	if p1.Shared == "" || p1.Shared != p2.Shared || p1.Shared != plan.Shared {
		t.Fatal("各请求共享前缀应字节相同")
	}
	if !strings.HasPrefix(p1.Unique, "[ab12cd34 c1-0] ") || !strings.HasPrefix(p2.Unique, "[ab12cd34 c4-3] ") {
		t.Error("唯一标记应紧跟在共享前缀之后")
	}
	if strings.Contains(p1.Shared, "ab12cd34 c") {
		t.Error("共享前缀里不得含唯一标记")
	}
	if got := nominalTokens(p1.Shared); math.Abs(float64(got-2000)) > 20 {
		t.Errorf("共享前缀 ≈ %d token，期望 ≈2000", got)
	}
	if got := nominalTokens(p1.Text()); math.Abs(float64(got-4000)) > 40 {
		t.Errorf("整条 prompt ≈ %d token，期望 ≈4000", got)
	}
	// 不同任务（nonce）的共享前缀不同：不吃上一个任务留下的缓存
	if buildPlan(w, "zz99yy88", 4).Shared == plan.Shared {
		t.Error("不同任务的共享前缀应不同")
	}
}

// cacheMock 模拟带前缀缓存的 openai_chat 上游：prompt_tokens = 字符数/4；
// cached_tokens = 与此前见过的 prompt 的最长公共前缀，按 64 token（256 字符）块对齐；
// 输出恰好写满 max_tokens。
type cacheMock struct {
	mu      sync.Mutex
	blocks  map[[32]byte]bool
	prompts []string
}

func (m *cacheMock) handle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	prompt := []rune(body.Messages[0].Content)
	m.mu.Lock()
	m.prompts = append(m.prompts, string(prompt))
	cached, hit := 0, true
	var h [32]byte
	for end := 256; end <= len(prompt); end += 256 {
		h = sha256.Sum256(append(h[:], []byte(string(prompt[end-256:end]))...))
		if hit && m.blocks[h] {
			cached += 64
		} else {
			hit = false
		}
		m.blocks[h] = true
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1、2\"}}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
	fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\n",
		len(prompt)/4, body.MaxTokens, cached)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func newCacheMock(t *testing.T) (*httptest.Server, *cacheMock) {
	t.Helper()
	m := &cacheMock{blocks: map[[32]byte]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(srv.Close)
	return srv, m
}

// TestPrepareWorkload 定标 + 写缓存：1 条定标测出字符/token 比，h>0 再串行 2 条写缓存，均记 warmup 样本
func TestPrepareWorkload(t *testing.T) {
	srv, mock := newCacheMock(t)
	params := StabilityParams{
		Protocol:         protocol.ProtocolOpenAIChat,
		Workload:         Workload{Input: InputSpec{Mode: InputFixed, Value: 4000}, CacheHitRate: 0.5},
		RequestTimeoutMs: 5000,
	}
	params.ApplyDefaults()
	in, samples, _, progressMax := ladderInput(t, srv.URL, params, NewCapGuard(0, 0, 0))
	in.Probe = WorkloadProbeID
	in.Nonce = "n0nce123"

	plan, err := PrepareWorkload(context.Background(), in)
	if err != nil {
		t.Fatalf("PrepareWorkload 出错: %v", err)
	}
	c := plan.Calibration
	if c == nil {
		t.Fatal("输入塑形时应产出定标结果")
	}
	if c.NominalRatio != nominalCharsPerToken || c.MeasuredRatio < 3.9 || c.MeasuredRatio > 4.1 {
		t.Errorf("名义/实测比 = %v/%v，期望 4/≈4（mock 按 4 字符 1 token 计）", c.NominalRatio, c.MeasuredRatio)
	}
	if c.PromptTokens <= 0 || c.Chars != calibChars {
		t.Errorf("定标 chars/promptTokens = %d/%d", c.Chars, c.PromptTokens)
	}
	if plan.SharedTokens != 2000 || c.SharedTokens != 2000 || plan.Shared == "" {
		t.Errorf("共享前缀 tokens = %d/%d，期望 2000", plan.SharedTokens, c.SharedTokens)
	}
	// 第 2 条写缓存请求应命中第 1 条写进去的共享前缀
	if c.CacheWarmCached == nil || *c.CacheWarmCached < 1900 {
		t.Errorf("写缓存第 2 条 cached = %v，期望 ≈2000", c.CacheWarmCached)
	}
	if len(mock.prompts) != 1+cacheWarmupRequests {
		t.Fatalf("上游收到 %d 条，期望 1 定标 + %d 写缓存", len(mock.prompts), cacheWarmupRequests)
	}
	if len(*samples) != 1+cacheWarmupRequests {
		t.Fatalf("样本 %d 条，期望 %d", len(*samples), 1+cacheWarmupRequests)
	}
	for _, s := range *samples {
		if !s.Warmup {
			t.Errorf("定标/写缓存样本 %s#%d 必须标 warmup", s.Stage, s.Seq)
		}
	}
	if (*samples)[0].Stage != stageCalib || (*samples)[1].Stage != stageCacheWrite {
		t.Errorf("样本档位 = %s/%s", (*samples)[0].Stage, (*samples)[1].Stage)
	}
	if *progressMax != 1+cacheWarmupRequests {
		t.Errorf("进度 = %d，期望 %d", *progressMax, 1+cacheWarmupRequests)
	}
	if EstPrepRequests(params) != 1+cacheWarmupRequests {
		t.Errorf("EstPrepRequests = %d", EstPrepRequests(params))
	}
}

// TestPrepareWorkloadNone 不塑形：不发任何请求，零值 Plan
func TestPrepareWorkloadNone(t *testing.T) {
	srv, mock := newCacheMock(t)
	var params StabilityParams
	params.Protocol = protocol.ProtocolOpenAIChat
	params.ApplyDefaults()
	in, _, _, _ := ladderInput(t, srv.URL, params, nil)
	plan, err := PrepareWorkload(context.Background(), in)
	if err != nil || plan.Calibration != nil || plan.Ratio != 0 {
		t.Fatalf("plan=%+v err=%v，期望零值", plan, err)
	}
	if len(mock.prompts) != 0 || EstPrepRequests(params) != 0 {
		t.Errorf("不塑形时不应发定标请求（实发 %d）", len(mock.prompts))
	}
}

// TestPrepareWorkloadNoUsage 渠道不回 usage 无法定标：fail-fast 报清原因，不带着错误比例跑完整个任务
func TestPrepareWorkloadNoUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"3\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	params := StabilityParams{Protocol: protocol.ProtocolOpenAIChat, Workload: Workload{Input: InputSpec{Mode: InputFixed, Value: 2000}}}
	params.ApplyDefaults()
	in, _, _, _ := ladderInput(t, srv.URL, params, nil)
	_, err := PrepareWorkload(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("err = %v，期望说明缺 usage", err)
	}
}

// TestLadderWorkloadEndToEnd 带缓存 mock 上跑阶梯：命中率落在 h±0.10、输入偏差 <10%、completion=输出目标，
// overall 带定标结果；h=0 时 cached 全 0
func TestLadderWorkloadEndToEnd(t *testing.T) {
	for _, h := range []float64{0.5, 0} {
		t.Run(fmt.Sprintf("h=%v", h), func(t *testing.T) {
			srv, _ := newCacheMock(t)
			params := StabilityParams{
				Protocol:          protocol.ProtocolOpenAIChat,
				ConcurrencyLadder: []int{1, 2},
				RequestsPerStage:  5,
				WarmupPerStage:    1,
				Workload:          Workload{Input: InputSpec{Mode: InputFixed, Value: 4000}, CacheHitRate: h, Output: 128},
				RequestTimeoutMs:  5000,
			}
			params.ApplyDefaults()
			in, samples, metrics, _ := ladderInput(t, srv.URL, params, NewCapGuard(0, 0, 0))
			in.Nonce = "n0nce123"
			plan, err := PrepareWorkload(context.Background(), in)
			if err != nil {
				t.Fatalf("PrepareWorkload: %v", err)
			}
			in.Plan = plan
			*samples = nil
			if err := runLadder(context.Background(), in); err != nil {
				t.Fatalf("runLadder: %v", err)
			}
			for _, s := range *samples {
				if s.TargetInputTokens != 4000 || s.TargetOutputTokens != 128 {
					t.Fatalf("样本目标 = %d/%d，期望 4000/128", s.TargetInputTokens, s.TargetOutputTokens)
				}
				if h == 0 && s.CachedTokens != 0 {
					t.Fatalf("h=0 时 %s#%d cached=%d，期望 0（唯一标记打头破缓存）", s.Stage, s.Seq, s.CachedTokens)
				}
			}
			overall := overallOf(t, *metrics)
			m := overall.Metrics
			if m.Calibration == nil {
				t.Error("overall 应带定标结果")
			}
			if m.InputDeviation == nil || math.Abs(m.InputDeviation.P50) >= 10 || m.InputDeviation.Exceeded {
				t.Errorf("输入偏差 = %+v，期望 |p50|<10%% 且未超阈值", m.InputDeviation)
			}
			if m.OutputDeviation == nil || m.OutputDeviation.P50 != 0 {
				t.Errorf("输出偏差 = %+v，期望 0（mock 恰好写满）", m.OutputDeviation)
			}
			if h == 0 {
				if m.CacheHitRate != nil || m.CacheHits != 0 {
					t.Errorf("h=0 不应断言命中率，cacheHits=%d", m.CacheHits)
				}
				return
			}
			if m.CacheHitRate == nil || math.Abs(*m.CacheHitRate-h) > cacheTolerance || m.CacheMiss {
				t.Errorf("命中率 = %v（miss=%v），期望 %v±%v", m.CacheHitRate, m.CacheMiss, h, cacheTolerance)
			}
			// h>0 时命中缓存是预期内的：样本照常进延迟分位
			if m.TTFTms == nil {
				t.Error("h>0 时命中缓存的样本应计入延迟分位")
			}
		})
	}
}

// TestTpmWeightFromWorkload TPM 每请求 token 权重 = 输入目标期望值 + 输出目标
func TestTpmWeightFromWorkload(t *testing.T) {
	p := StabilityParams{Workload: Workload{Input: InputSpec{Mode: InputRamp, Min: 1000, Max: 3000}, Output: 500}}
	if got := tpmWeightPerReq(p); got != 2500 {
		t.Errorf("weight = %v，期望 2000+500", got)
	}
	// 不塑形：输入按小 prompt 的名义 token 估，输出沿用 TPM 默认 256
	var def StabilityParams
	def.ApplyDefaults()
	want := float64(nominalTokens("[xxxxxxxx t0-0] "+countPrompt) + DefaultTpmMaxTokens)
	if got := tpmWeightPerReq(def); got != want {
		t.Errorf("默认 weight = %v，期望 %v", got, want)
	}
}
