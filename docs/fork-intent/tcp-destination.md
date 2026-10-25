# DT-01：TCP 保留 DNSMapping 的真實目的 IP

2026-10-25，基準 `dc902107302505db81477f624eb864a36f14098a`。
這是獨立的 TCP 目的地址修正，不改變已記錄的上游同步基準。

## 故障與契約

ViuTV API 回傳的播放網址綁定請求出口 IP。正式 router 的 API 連線已持有正確
`destinationIP`、`dnsMode=redir-host` 及空 `sniffHost`，仍帶 DNS 回填的 `Host`。
TCP 撥號未呼叫 `Metadata.Pure()`，Shadowsocks 因而把域名送到上游重新解析；
同一節點的 domain／IP 連線控制實驗產生不同 IP claim，前者 CDN 403、後者清單 200。
上游 [issue #2997](https://github.com/MetaCubeX/mihomo/issues/2997) 記錄相同的目的地址選擇缺口。

規則可以使用 DNS 回填的域名，但 TCP 出站應保留已知的真實 IP。對
`DNSMapping`／`DNSHosts` 且具有效 IP 的 metadata，使用既有 `Pure()`；
`DNSNormal` 的顯式域名與 `DNSFakeIP` 繼續把域名交給代理。
成功嗅探已明確覆寫目的地時仍依原來的 `DNSNormal` 行為。

## 實作與影響

[TCP 路徑](../../tunnel/tunnel.go) 在完成規則選擇後，以 `metadata.Pure()` 建立
撥號 metadata。原 metadata 的域名供規則／日誌使用；既有的 tracker 建立前
Host 恢復保留。沒有加入服務名稱、固定 IP、新配置開關、重試或選路機制。

SC-01 至 SC-03 的連線保留、選舉與故障分類程式未改動。SC-04／SC-05 的
wrapper 與 tracker 使用同一個撥號 metadata；`Pure()` 只複製並清除 Host，其他
Smart 欄位保留，現有 tracker 前 Host 恢復仍執行。CI-01 的 workflow 未改動。
這些是程式審查結果，真實流量改善須另行驗證。

## 驗證與維護

[回歸測試](../../tunnel/tcp_destination_test.go) 經完整 TCP tunnel、域名規則、真實
loopback SOCKS handshake 及 client ping 回應觀察目的地址。未修正版本在 mapped
IPv4／IPv6 兩例送出域名並失敗；修正後兩例成功，顯式域名與 fake-IP 兩例維持成功。
測試同時檢查原規則 metadata 的 Host 保留。

```bash
go test ./tunnel -run TestTCPPreservesDNSMappedDestination -count=1 -v
```

本機同時通過 tunnel／Smart 專項 race，以及一般／`with_gvisor` 完整套件，後兩者
沿用 `CGO_ENABLED=0 SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1`，不涵蓋被跳過的
情境。一般套件首次在兩個 QUIC inbound 測試失敗；舊版乾淨 archive 的同兩項與
未改程式的候選完整重跑均通過。首次失敗日誌保留，尚未判定其瞬時原因。

原始比較證據位於 macOSAgentBot 的
`.runtime/viutv-live/cdn403-20261025/core-tcp-fix/`，屬本機未追蹤產物。
隔離 ARM64 舊／新核心使用同一份配置（SHA-256
`febbf53d075f214fd1f05b68e41d9e1c8766c23e2baf0d770f9fbbef22f7e57e`）、
同一 leaf node、相同 DNS 答案及 CONNECT IP，保留原 API SNI／Host 與 TLS 完整驗證。
兩者皆真實建立 `redir-host` mapping。舊核心 API 的 IP claim 與獨立出口量測不符，
CDN 403／空 body；候選與出口量測一致，原 URL 經官方 302 後主清單及 variant 均 200，
variant 為 FairPlay／SAMPLE-AES。沒有修改或驗證播放網址的密碼學簽名，也未解密影片。

這個受控等價類使用正式已有的 Cloudflare DoH resolver，隔離 exact API policy 固定同一
出口；不是完整正式 DNS 配置驗收。前兩組不同 resolver 給出的端點出現 TLS timeout／
取證等待斷線，均保留為 ERROR，沒有混入最後的 binary 比較。原始安全結果為
`worker-121724/mapping-comparison/installed-cloudflare-auto.json` 及
`candidate-cloudflare-auto.json`；主代理另以檔案 hash、TLS 與 HTTP 狀態獨立核對。

目前尚未以候選替換正式核心。發布後須經正式 router 更新，再測原生 Mac／container
取流與 Safari 99／96 的真實授權及影格輸出；記錄公開 binary SHA、更新結果與回滾來源。
