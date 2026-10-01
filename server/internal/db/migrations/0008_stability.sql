-- +goose Up
-- 稳定性检测逐请求原始时序：一行 = 一次压测请求（证据链 + 评估期确定性算分位数的源）。
-- 与 connectivity_results 同范式：显式时间戳、随任务 cascade 删除、失败字段留 NULL。
-- 与质量检测的 task_case_results 刻意分表：稳定性产出的是时序性能指标，装不进
-- 「用例×模式→passed 占比」的合规判定模型。
create table stability_samples (
    id            bigint generated always as identity primary key,
    task_id       uuid not null references tasks (id) on delete cascade,
    probe         text not null,             -- concurrency_ladder | rpm_probe | tpm_probe
    stage         text not null,             -- 档位标识：并发档 'c8'、目标速率档 'r120'
    stage_index   int not null,              -- 档在本 probe 内的序号（0 起，供图表 x 轴稳定排序）
    seq           int not null,              -- 档内请求序号（0 起）
    protocol      text not null,
    -- open-loop 排定发起时刻（非实际起飞）：闭环阶梯下 = 实际发起；用于测协调遗漏
    dispatched_at timestamptz not null,
    ttfb_ms       int,                       -- 首字节耗时（未拿到 200 响应体留 NULL）
    ttfd_ms       int,                       -- 首个非空增量（推理或正文）耗时（无增量留 NULL）
    ttft_ms       int,                       -- 首个非空正文增量耗时（无正文留 NULL）
    total_ms      int,                       -- 整请求耗时（未拿到 200 响应体留 NULL）
    ok            boolean not null,
    http_status   int,
    http_proto    text,                      -- 实际协商的协议版本（如 HTTP/1.1；传输层失败留 NULL）
    -- 错误分类一等公民：ok=true 时留 NULL；失败必落其一，评估期按类堆叠
    error_class   text,
    error         text,
    input_tokens  int,                       -- usage.prompt（无则 NULL）
    output_tokens int,                       -- usage.completion（无则 NULL）
    cached_tokens int,                       -- 输入中命中缓存的 token（无 usage 则 NULL；>0 的样本不进延迟分位）
    warmup        boolean not null default false,  -- 预热样本，评估时剔除
    -- budget_exhausted = 生成上限被推理耗尽仍无正文（砝码不足，不计入错误率）；
    -- reasoning_only = 只有推理没有正文就正常结束（渠道/模型问题）
    check (error_class is null or error_class in
        ('transport', 'http_4xx', 'rate_limited', 'http_5xx', 'stream_anomaly', 'semantic_empty',
         'budget_exhausted', 'reasoning_only'))
);

create index stability_samples_task_probe_stage on stability_samples (task_id, probe, stage_index, seq);

-- 稳定性检测评估期聚合点：一行 = 一个（probe × 档位）的指标快照，驱动图表与结论。
-- stage='__overall__' 存 probe 级总结论（如收敛的 RPM/TPM 边界、拐点并发）。
-- 指标全放 jsonb：不同 probe 的指标集差异大（阶梯有分位数曲线、RPM/TPM 有收敛边界），
-- 硬编成列会稀疏且频繁加列；jsonb 一处收口，前端按 probe 类型取用。
create table stability_metrics (
    id          bigint generated always as identity primary key,
    task_id     uuid not null references tasks (id) on delete cascade,
    probe       text not null,
    stage       text not null,             -- 档位标识或 '__overall__'
    stage_index int not null,              -- 档序（__overall__ 取 -1，排在最后）
    metrics     jsonb not null,
    created_at  timestamptz not null default now(),
    unique (task_id, probe, stage)
);

create index stability_metrics_task on stability_metrics (task_id, probe, stage_index);

-- +goose Down
drop table stability_metrics;
drop table stability_samples;
