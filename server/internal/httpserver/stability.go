package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/db"
	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/stability"
	stabreg "github.com/Yukiho0287/assay/server/internal/probe/stability/registry"
	"github.com/Yukiho0287/assay/server/internal/tasks"
	"github.com/Yukiho0287/assay/server/internal/version"
)

// taskKindStability tasks.kind 取值：稳定性检测大类
const taskKindStability = "stability"

// stabilityFootnotes 报告口径脚注：测量原点/分位数算法/预热剔除等，随报告与导出下发（证据链自足）。
var stabilityFootnotes = []string{
	"TTFB=响应体首个非空字节到达耗时；TTFD=首个非空增量（推理或正文）到达耗时；TTFT=首个非空正文增量到达耗时；均以平台发出请求为唯一测量原点。",
	"请求固定走 HTTP/1.1、多连接（对齐官方 SDK 默认行为），每条样本记录实际协商的协议版本。",
	"每条 prompt 带任务内唯一前缀，破渠道整条响应缓存；输入命中缓存（cached_tokens>0）的成功样本不计入延迟分位。",
	"成功 = HTTP 200 + 流走到协议结束帧 + 有正文；200 之后流被掐断、无结束帧或流内报错计为流异常。",
	"生成上限被推理耗尽仍无正文计为「预算耗尽」：是测试砝码太小而非渠道故障，不计入错误率；出现即说明 TTFT 结论不可用，应调大生成上限。",
	"分位数在评估期对成功样本确定性计算（线性插值，对齐 numpy type-7 / vLLM bench 口径）。",
	"预热样本（warmup）不计入任何指标：阶梯并发每档开头的预热请求，以及 RPM/TPM 每档前一半的热身期。",
	"吞吐（rps / tokens·s⁻¹）按样本时间跨度计（最早排定至最晚完成）；__overall__ 行不含跨档吞吐。",
	"RPM 实测为开环恒定到达率：dispatched_at 记排定时刻而非起飞时刻（修正协调遗漏）；每档前一半为热身（让渠道消化上一档没清零的计数、令牌桶攒下的突发额度），只用后一半的 429 占比判定是否限速，二分收敛可持续 RPM 边界。",
	"TPM 实测为开环恒定 token 到达率：每请求 max_tokens 砝码 + 顶格数数 prompt 打满输出，按「输入+输出都计」的每请求 token 权重换算请求速率发压；热身与判定同 RPM，实测 token 吞吐取判定段响应 usage 真实值，二分收敛可持续 TPM 边界。",
	"收敛值标「截断」表示搜索撞上总请求/总 token/整任务时长上限：尚未出现限速档时它只是下界（真实边界 ≥ 该值），已出现限速档时二分未完成、精度不足。",
}

func (h *handlers) ListStabilityProbes(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	items := make([]api.StabilityProbeInfo, 0, len(stabreg.All()))
	for _, p := range stabreg.All() {
		items = append(items, stabilityProbeInfoToAPI(p.Info))
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handlers) CreateStabilityTask(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePerm(w, r, "stability")
	if !ok {
		return
	}
	var req api.StabilityTaskCreate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "请求格式错误"})
		return
	}
	if len(req.Probes) == 0 {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "至少勾选一个检测项"})
		return
	}
	probes := make([]stability.Probe, 0, len(req.Probes))
	for _, id := range req.Probes {
		p, ok := stabreg.Get(id)
		if !ok {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: fmt.Sprintf("未知检测项 %q", id)})
			return
		}
		probes = append(probes, p)
	}

	// 参数落默认 + 范围校验（含协议非空）——问题在创建时暴露，不带进执行期
	params, errMsg := resolveStabilityParams(req.Params)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: errMsg})
		return
	}

	ch, err := h.q.GetChannel(r.Context(), req.ChannelId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "渠道不存在"})
			return
		}
		h.internalError(w, "读取渠道失败", err)
		return
	}
	if ch.Disabled {
		writeJSON(w, http.StatusConflict, api.Error{Error: "渠道已停用，不能发起新任务"})
		return
	}
	// 实选协议必须是渠道声明协议之一
	if !slices.Contains(ch.Protocols, params.Protocol) {
		writeJSON(w, http.StatusConflict, api.Error{Error: fmt.Sprintf("渠道未声明协议 %s", params.Protocol)})
		return
	}
	m, err := h.q.GetChannelModel(r.Context(), db.GetChannelModelParams{ID: req.ModelEntryId, ChannelID: req.ChannelId})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "模型条目不存在"})
			return
		}
		h.internalError(w, "读取模型条目失败", err)
		return
	}
	// 每个检测项都必须适用实选协议（Protocols 为空=全适用）
	for _, p := range probes {
		if len(p.Info.Protocols) > 0 && !slices.Contains(p.Info.Protocols, params.Protocol) {
			writeJSON(w, http.StatusConflict, api.Error{
				Error: fmt.Sprintf("检测项「%s」不支持协议 %s", p.Info.Name, params.Protocol),
			})
			return
		}
	}

	// progress_total 取最坏预估上界；probe 碰硬闸提前收敛则实发少于此值，任务照常 succeeded
	total := 0
	for _, p := range probes {
		total += p.Info.EstRequests(params)
	}

	// 参数快照：证据链要求，历史报告不受渠道后续编辑/删除影响（绝不含 API key）
	target := probe.Target{
		ChannelID:        ch.ID.String(),
		ChannelName:      ch.Name,
		BaseURL:          ch.BaseUrl,
		ModelEntryID:     m.ID.String(),
		Model:            m.Name,
		Protocols:        ch.Protocols,
		Currency:         ch.Currency,
		InputPrice:       m.InputPrice,
		OutputPrice:      m.OutputPrice,
		CachedInputPrice: m.CachedInputPrice,
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		h.internalError(w, "序列化任务快照失败", err)
		return
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		h.internalError(w, "序列化任务参数失败", err)
		return
	}

	// 任务行与队列条目同一事务：要么都在要么都不在
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		h.internalError(w, "开启事务失败", err)
		return
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	qtx := h.q.WithTx(tx)
	row, err := qtx.CreateTask(r.Context(), db.CreateTaskParams{
		Kind:          taskKindStability,
		ChannelID:     pgtype.UUID{Bytes: req.ChannelId, Valid: true},
		Target:        targetJSON,
		Probes:        req.Probes,
		Params:        paramsJSON,
		ProgressTotal: int32(total),
		CreatedBy:     pgtype.UUID{Bytes: s.ID, Valid: true},
	})
	if err != nil {
		h.internalError(w, "创建任务失败", err)
		return
	}
	jobID, err := h.tq.EnqueueStabilityTaskTx(r.Context(), tx, row.ID)
	if err != nil {
		h.internalError(w, "任务入队失败", err)
		return
	}
	if err := qtx.SetTaskRiverJobID(r.Context(), db.SetTaskRiverJobIDParams{
		ID: row.ID, RiverJobID: pgtype.Int8{Int64: jobID, Valid: true},
	}); err != nil {
		h.internalError(w, "回写队列 ID 失败", err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.internalError(w, "提交事务失败", err)
		return
	}

	h.log.Info("稳定性检测任务已创建", "task", row.ID, "channel", ch.Name, "model", m.Name,
		"protocol", params.Protocol, "probes", req.Probes, "est", total)
	task, err := h.q.GetTask(r.Context(), row.ID)
	if err != nil {
		h.internalError(w, "读取新建任务失败", err)
		return
	}
	out, err := stabilityTaskToAPI(task)
	if err != nil {
		h.internalError(w, "任务数据损坏", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *handlers) ListStabilityTasks(w http.ResponseWriter, r *http.Request, params api.ListStabilityTasksParams) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	limit, offset := 50, 0
	if params.Limit != nil && *params.Limit >= 1 && *params.Limit <= 200 {
		limit = *params.Limit
	}
	if params.Offset != nil && *params.Offset > 0 {
		offset = *params.Offset
	}
	// 筛选条件：nil = 不过滤；列表与总数用同一组条件，否则页码算错
	var status *string
	if params.Status != nil {
		status = (*string)(params.Status)
	}
	var channelID pgtype.UUID
	if params.ChannelId != nil {
		channelID = pgtype.UUID{Bytes: *params.ChannelId, Valid: true}
	}
	rows, err := h.q.ListTasks(r.Context(), db.ListTasksParams{
		Kind: taskKindStability, Limit: int32(limit), Offset: int32(offset),
		Status: status, ChannelID: channelID,
	})
	if err != nil {
		h.internalError(w, "读取任务列表失败", err)
		return
	}
	total, err := h.q.CountTasks(r.Context(), db.CountTasksParams{
		Kind: taskKindStability, Status: status, ChannelID: channelID,
	})
	if err != nil {
		h.internalError(w, "统计任务数失败", err)
		return
	}
	items := make([]api.StabilityTask, 0, len(rows))
	for _, row := range rows {
		t, err := stabilityTaskToAPI(db.GetTaskRow(row))
		if err != nil {
			h.internalError(w, "任务数据损坏", err)
			return
		}
		items = append(items, t)
	}
	writeJSON(w, http.StatusOK, api.StabilityTaskList{Items: items, Total: int(total)})
}

func (h *handlers) GetStabilityTask(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	task, ok := h.loadStabilityTask(w, r, id)
	if !ok {
		return
	}
	out, err := stabilityTaskToAPI(task)
	if err != nil {
		h.internalError(w, "任务数据损坏", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) CancelStabilityTask(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	task, ok := h.loadStabilityTask(w, r, id)
	if !ok {
		return
	}
	rows, err := h.q.CancelTask(r.Context(), id)
	if err != nil {
		h.internalError(w, "取消任务失败", err)
		return
	}
	if rows == 0 {
		writeJSON(w, http.StatusConflict, api.Error{Error: "任务已结束，不可取消"})
		return
	}
	// 任务行已翻 canceled（权威状态），再取消 river job：排队的原子取消；
	// 运行中的通知 worker 取消 ctx 中止在途请求。失败只告警不回滚——状态守卫兜底。
	if task.RiverJobID.Valid {
		if err := h.tq.CancelJob(r.Context(), task.RiverJobID.Int64); err != nil {
			h.log.Warn("取消 river job 失败（任务行已取消，靠状态守卫兜底）", "task", id, "job", task.RiverJobID.Int64, "err", err)
		}
	}
	h.broker.notify(r.Context(), id, "canceled", int(task.ProgressDone), int(task.ProgressTotal))
	h.log.Info("稳定性任务已取消", "task", id)

	task, err = h.q.GetTask(r.Context(), id)
	if err != nil {
		h.internalError(w, "读取任务失败", err)
		return
	}
	out, err := stabilityTaskToAPI(task)
	if err != nil {
		h.internalError(w, "任务数据损坏", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) GetStabilityTaskMetrics(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	task, ok := h.loadStabilityTask(w, r, id)
	if !ok {
		return
	}
	if !isTerminalTask(task.Status) {
		writeJSON(w, http.StatusConflict, api.Error{Error: "任务尚未结束，暂无指标报告"})
		return
	}
	report, ok := h.buildStabilityReport(w, r, task)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (h *handlers) ExportStabilityTask(w http.ResponseWriter, r *http.Request, id api.IdPath, params api.ExportStabilityTaskParams) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	task, ok := h.loadStabilityTask(w, r, id)
	if !ok {
		return
	}
	if !isTerminalTask(task.Status) {
		writeJSON(w, http.StatusConflict, api.Error{Error: "任务尚未结束，暂无可导出报告"})
		return
	}
	if params.Format != api.ExportStabilityTaskParamsFormatJson {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "未知导出格式"})
		return
	}

	apiTask, err := stabilityTaskToAPI(task)
	if err != nil {
		h.internalError(w, "任务数据损坏", err)
		return
	}
	report, ok := h.buildStabilityReport(w, r, task)
	if !ok {
		return
	}
	sampleRows, err := h.q.ListStabilitySamples(r.Context(), id)
	if err != nil {
		h.internalError(w, "读取样本失败", err)
		return
	}
	samples := make([]api.StabilitySample, 0, len(sampleRows))
	for _, row := range sampleRows {
		samples = append(samples, stabilitySampleToAPI(row))
	}
	export := api.StabilityExport{
		Tool:    "assay",
		Version: version.Version,
		Task:    apiTask,
		Report:  report,
		Samples: samples,
	}
	// 文件名带任务 id 前 8 位，多任务导出不互相覆盖
	stem := fmt.Sprintf("assay-stability-%s", task.ID.String()[:8])
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", stem+".json"))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(export); err != nil {
		h.log.Error("写出 JSON 导出失败", "err", err)
	}
}

func (h *handlers) StreamStabilityTaskEvents(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	if _, ok := h.requirePerm(w, r, "stability"); !ok {
		return
	}
	if _, ok := h.loadStabilityTask(w, r, id); !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务不支持事件流"})
		return
	}

	// 先订阅、再读快照：快照与订阅之间不留事件真空
	ch, cancel := h.broker.subscribe(id)
	defer cancel()
	task, err := h.q.GetTask(r.Context(), id)
	if err != nil {
		h.internalError(w, "读取任务快照失败", err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // OpenResty：关代理缓冲，事件即时下发
	w.WriteHeader(http.StatusOK)

	snapshot, err := json.Marshal(tasks.Event{
		TaskID: task.ID,
		Status: task.Status,
		Done:   int(task.ProgressDone),
		Total:  int(task.ProgressTotal),
	})
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", snapshot)
	fl.Flush()
	if isTerminalStatus(task.Status) {
		return
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case payload := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", payload)
			fl.Flush()
			var ev struct {
				Status string `json:"status"`
			}
			if json.Unmarshal([]byte(payload), &ev) == nil && isTerminalStatus(ev.Status) {
				return
			}
		}
	}
}

// buildStabilityReport 读 stability_metrics 组装档级+overall 指标报告；metrics jsonb 与
// api.StabilityMetrics 的 json 标签一一对应，直接反序列化。ok=false 表示已写错误响应。
func (h *handlers) buildStabilityReport(w http.ResponseWriter, r *http.Request, task db.GetTaskRow) (api.StabilityReport, bool) {
	rows, err := h.q.ListStabilityMetrics(r.Context(), task.ID)
	if err != nil {
		h.internalError(w, "读取指标失败", err)
		return api.StabilityReport{}, false
	}
	stages := make([]api.StabilityStageMetric, 0, len(rows))
	for _, row := range rows {
		var m api.StabilityMetrics
		if err := json.Unmarshal(row.Metrics, &m); err != nil {
			h.internalError(w, "指标数据损坏", err)
			return api.StabilityReport{}, false
		}
		stages = append(stages, api.StabilityStageMetric{
			Probe:      row.Probe,
			Stage:      row.Stage,
			StageIndex: int(row.StageIndex),
			Metrics:    m,
		})
	}
	var params api.StabilityTaskParams
	if err := json.Unmarshal(task.Params, &params); err != nil {
		h.internalError(w, "任务参数损坏", err)
		return api.StabilityReport{}, false
	}
	var proto api.Protocol
	if params.Protocol != nil {
		proto = *params.Protocol
	}
	footnotes := stabilityFootnotes
	report := api.StabilityReport{
		TaskId:      task.ID,
		Status:      api.TaskStatus(task.Status),
		Protocol:    proto,
		Stages:      stages,
		Footnotes:   &footnotes,
		GeneratedAt: time.Now().UTC(),
	}
	if task.Status != "succeeded" {
		v := true
		report.Incomplete = &v
	}
	return report, true
}

// loadStabilityTask 读任务并校验 kind，查无此任务（或不是稳定性任务）统一 404。
func (h *handlers) loadStabilityTask(w http.ResponseWriter, r *http.Request, id api.IdPath) (db.GetTaskRow, bool) {
	task, err := h.q.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "任务不存在"})
			return db.GetTaskRow{}, false
		}
		h.internalError(w, "读取任务失败", err)
		return db.GetTaskRow{}, false
	}
	if task.Kind != taskKindStability {
		writeJSON(w, http.StatusNotFound, api.Error{Error: "任务不存在"})
		return db.GetTaskRow{}, false
	}
	return task, true
}

// resolveStabilityParams api 入参 → 领域参数，落默认 + fail-fast 校验；非空 errMsg = 400。
func resolveStabilityParams(in api.StabilityTaskParams) (stability.StabilityParams, string) {
	var p stability.StabilityParams
	if in.Protocol != nil {
		p.Protocol = string(*in.Protocol)
	}
	if in.ConcurrencyLadder != nil {
		p.ConcurrencyLadder = *in.ConcurrencyLadder
	}
	if in.RequestsPerStage != nil {
		p.RequestsPerStage = *in.RequestsPerStage
	}
	if in.WarmupPerStage != nil {
		p.WarmupPerStage = *in.WarmupPerStage
	}
	if in.LadderMaxTokens != nil {
		p.LadderMaxTokens = *in.LadderMaxTokens
	}
	if in.RpmStartRate != nil {
		p.RpmStartRate = float64(*in.RpmStartRate)
	}
	if in.RpmMaxRate != nil {
		p.RpmMaxRate = float64(*in.RpmMaxRate)
	}
	if in.RpmStageSec != nil {
		p.RpmStageSec = *in.RpmStageSec
	}
	if in.RpmMaxInFlight != nil {
		p.RpmMaxInFlight = *in.RpmMaxInFlight
	}
	if in.RpmMaxTokens != nil {
		p.RpmMaxTokens = *in.RpmMaxTokens
	}
	if in.RpmLimitThreshold != nil {
		p.RpmLimitThreshold = float64(*in.RpmLimitThreshold)
	}
	if in.RpmBinarySteps != nil {
		p.RpmBinarySteps = *in.RpmBinarySteps
	}
	if in.TpmStartRate != nil {
		p.TpmStartRate = float64(*in.TpmStartRate)
	}
	if in.TpmMaxRate != nil {
		p.TpmMaxRate = float64(*in.TpmMaxRate)
	}
	if in.TpmStageSec != nil {
		p.TpmStageSec = *in.TpmStageSec
	}
	if in.TpmMaxInFlight != nil {
		p.TpmMaxInFlight = *in.TpmMaxInFlight
	}
	if in.TpmMaxTokensPerReq != nil {
		p.TpmMaxTokensPerReq = *in.TpmMaxTokensPerReq
	}
	if in.TpmLimitThreshold != nil {
		p.TpmLimitThreshold = float64(*in.TpmLimitThreshold)
	}
	if in.TpmBinarySteps != nil {
		p.TpmBinarySteps = *in.TpmBinarySteps
	}
	if in.MaxTotalRequests != nil {
		p.MaxTotalRequests = *in.MaxTotalRequests
	}
	if in.MaxTotalTokens != nil {
		p.MaxTotalTokens = *in.MaxTotalTokens
	}
	if in.RequestTimeoutMs != nil {
		p.RequestTimeoutMs = *in.RequestTimeoutMs
	}
	p.ApplyDefaults()
	if err := p.Validate(); err != nil {
		return p, err.Error()
	}
	return p, ""
}

func stabilityProbeInfoToAPI(info stability.Info) api.StabilityProbeInfo {
	protocols := make([]api.Protocol, 0, len(info.Protocols))
	for _, p := range info.Protocols {
		protocols = append(protocols, api.Protocol(p))
	}
	// 用默认参数算最坏预估请求数（前端据实选参数自行重算展示）
	var def stability.StabilityParams
	def.ApplyDefaults()
	est := 0
	if info.EstRequests != nil {
		est = info.EstRequests(def)
	}
	return api.StabilityProbeInfo{
		Id:          info.ID,
		Name:        info.Name,
		Description: info.Description,
		Protocols:   protocols,
		EstRequests: est,
	}
}

func stabilityTaskToAPI(t db.GetTaskRow) (api.StabilityTask, error) {
	var target api.TaskTarget
	if err := json.Unmarshal(t.Target, &target); err != nil {
		return api.StabilityTask{}, fmt.Errorf("解析任务快照: %w", err)
	}
	var params api.StabilityTaskParams
	if err := json.Unmarshal(t.Params, &params); err != nil {
		return api.StabilityTask{}, fmt.Errorf("解析任务参数: %w", err)
	}
	out := api.StabilityTask{
		Id:            t.ID,
		Status:        api.TaskStatus(t.Status),
		Target:        target,
		Params:        params,
		Probes:        t.Probes,
		ProgressTotal: int(t.ProgressTotal),
		ProgressDone:  int(t.ProgressDone),
		Error:         t.Error,
		CreatedAt:     t.CreatedAt,
		CreatedBy:     t.CreatedByName,
	}
	if t.StartedAt.Valid {
		v := t.StartedAt.Time
		out.StartedAt = &v
	}
	if t.FinishedAt.Valid {
		v := t.FinishedAt.Time
		out.FinishedAt = &v
	}
	return out, nil
}

func stabilitySampleToAPI(r db.ListStabilitySamplesRow) api.StabilitySample {
	s := api.StabilitySample{
		Probe:        r.Probe,
		Stage:        r.Stage,
		StageIndex:   int(r.StageIndex),
		Seq:          int(r.Seq),
		Protocol:     api.Protocol(r.Protocol),
		DispatchedAt: r.DispatchedAt,
		Ok:           r.Ok,
		Warmup:       r.Warmup,
		ErrorClass:   r.ErrorClass,
		Error:        r.Error,
	}
	if r.HttpStatus.Valid {
		v := int(r.HttpStatus.Int32)
		s.HttpStatus = &v
	}
	if r.TtfbMs.Valid {
		v := int(r.TtfbMs.Int32)
		s.TtfbMs = &v
	}
	if r.TtftMs.Valid {
		v := int(r.TtftMs.Int32)
		s.TtftMs = &v
	}
	if r.TotalMs.Valid {
		v := int(r.TotalMs.Int32)
		s.TotalMs = &v
	}
	if r.InputTokens.Valid {
		v := int(r.InputTokens.Int32)
		s.InputTokens = &v
	}
	if r.OutputTokens.Valid {
		v := int(r.OutputTokens.Int32)
		s.OutputTokens = &v
	}
	return s
}
