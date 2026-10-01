-- +goose Up
-- 飞书 OAuth 登录：union_id 是同一开发者下跨应用稳定的用户标识，作唯一键；
-- open_id 仅在本应用内唯一（换应用会变），备存供排查与调用飞书接口用。
alter table users
    add column feishu_union_id text unique,
    add column feishu_open_id  text,
    add column display_name    text,
    add column avatar_url      text;

-- 飞书自动建号的账号没有密码：放开非空约束。
-- password_hash 为空 = 该账号不可走密码登录（密码登录只留给管理员兜底）。
alter table users alter column password_hash drop not null;

-- +goose Down
-- ⚠️ 仅供开发回滚：飞书账号没有密码，恢复非空约束前必须先删掉它们。
delete from users where password_hash is null;
alter table users alter column password_hash set not null;
alter table users
    drop column avatar_url,
    drop column display_name,
    drop column feishu_open_id,
    drop column feishu_union_id;
