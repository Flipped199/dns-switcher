## DNS Switcher

用以检测反代宕机时自动切换到CloudFlare代理，当反代再次上线时，切换回去
依靠`health_check_url`进行http检测，检测成功的标准是`StatusCode >= 200 && StatusCode < 300`

我们设计成，当每个记录项的`health_check_url`相同时，应当合并成同一个检测任务。

配置项
```toml
cloudflare_api_token = ""
cloudflare_zone_id = ""
check_interval = 10 # second

# api
[[dns_records]]
type = "A"
dns_record_id = "xxx"
health_check_url = "http://0.0.0.0/abc"
content = "1.2.3.4"
proxied = false
[dns_records.fallback]
content = "4.5.6.7"
proxied = true

# @
[[dns_records]]
type = "A"
dns_record_id = "xxx"
health_check_url = "http://0.0.0.0/abc"
content = "1.2.3.4"
proxied = false
[dns_records.fallback]
content = "4.5.6.7"
proxied = true
```

为了尽可能的简单，仅允许修改记录类型与记录值，其他值保持不变，如需修改应前往CloudFlare面板手动修改

运行
```shell
docker run --name dns-switcher -d -v $PWD/config.toml:/app/config.toml dns-switcher:latest
```