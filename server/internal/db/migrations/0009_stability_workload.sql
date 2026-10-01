-- +goose Up
-- 负载画像：逐请求记下输入/输出 token 目标，报告据此算「目标 vs 实测」偏差分位。
-- 定标/写缓存请求挂在伪检测项 probe='workload'（stage calib/cache，均为 warmup），不出指标只留证据。
alter table stability_samples
    add column target_input_tokens  int,  -- 输入 token 目标（不塑形留 NULL）
    add column target_output_tokens int;  -- 输出 token 目标（无目标留 NULL）

-- 旧任务快照改写：三个 probe 各自的生成上限（ladderMaxTokens / rpmMaxTokens / tpmMaxTokensPerReq）
-- 合并为 workload.output（设了 = 三个 probe 一律用它，不设 = 各 probe 默认 2048/16/256）。
-- 只看任务实际跑过的 probe：全是默认值 → 删键即可；非默认值只有一种 → workload.output = 它；
-- 否则新模型表达不了，中止迁移（宁可停在这里人工处理，也不悄悄改掉历史快照的语义）。
-- +goose StatementBegin
do $$
declare
    t    record;
    vals int[];
    nondefault boolean;
begin
    for t in select id, probes, params from tasks where kind = 'stability' loop
        vals := array[]::int[];
        nondefault := false;
        if 'concurrency_ladder' = any (t.probes) then
            vals := vals || coalesce((t.params ->> 'ladderMaxTokens')::int, 2048);
            nondefault := nondefault or vals[array_length(vals, 1)] <> 2048;
        end if;
        if 'rpm_probe' = any (t.probes) then
            vals := vals || coalesce((t.params ->> 'rpmMaxTokens')::int, 16);
            nondefault := nondefault or vals[array_length(vals, 1)] <> 16;
        end if;
        if 'tpm_probe' = any (t.probes) then
            vals := vals || coalesce((t.params ->> 'tpmMaxTokensPerReq')::int, 256);
            nondefault := nondefault or vals[array_length(vals, 1)] <> 256;
        end if;
        if nondefault and (select count(distinct v) from unnest(vals) v) > 1 then
            raise exception '任务 % 各检测项生成上限不一致 %（probes=%），无法合并为 workload.output，需人工处理',
                t.id, vals, t.probes;
        end if;
        update tasks
        set params = (params - 'ladderMaxTokens' - 'rpmMaxTokens' - 'tpmMaxTokensPerReq')
            || case when nondefault
                    then jsonb_build_object('workload', jsonb_build_object('output', vals[1]))
                    else '{}'::jsonb end
        where id = t.id;
    end loop;
end
$$;
-- +goose StatementEnd

-- +goose Down
-- 快照改写是单向的（旧的三键无法从 workload.output 无损还原），回滚只删列
alter table stability_samples
    drop column target_output_tokens,
    drop column target_input_tokens;
