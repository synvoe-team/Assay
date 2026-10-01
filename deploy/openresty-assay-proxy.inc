# Assay 反代片段（安装到 /opt/1panel/www/conf.d/assay-proxy.inc）
# 后缀用 .inc 而非 .conf：nginx.conf 只 include conf.d/*.conf，避免被当成独立 server 重复加载。
# 8321（明文，兜底直连）与 443（CDN 回源）两个 server 块共用这一份，改一处即可。
location / {
    proxy_pass http://assay_backend;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $assay_forwarded_proto;
    proxy_set_header Connection "";
    proxy_buffering off;      # SSE 进度推送不缓冲
    proxy_read_timeout 3600s; # 检测任务 SSE 长连接
    client_max_body_size 50m;
}
