# Smart 連線行為意圖

狀態為已實作。2026-11-15 更新 SC-01 至 SC-05。共同基準、歷史提交與驗證成熟度見[索引](README.md)。

本文件區分需要保留的行為、目前實作方法與尚未證明的部分。常數和函式可以隨上游重構改變；改變判定門檻或影響範圍時，必須同步更新意圖及證據。

## SC-01 成功撥號不淘汰既有連線

**問題與目的：** 上游會在 TCP／UDP 撥號成功後呼叫 `closeSameConnection(..., false)`，關閉同 SmartTarget、同組內不經新 winner 的其他連線。新連線競速勝出本身並不能證明舊連線故障；不應因此中斷既有串流。

**必須保留：** TCP `DialContext` 與 UDP `ListenPacketContext` 的成功路徑，不得只因新 winner 不同就淘汰其他已建立的連線。winner 記錄、成功統計及 metric wrapper 仍須正常完成。

TCP 與 UDP 成功後會更新勝者及連線統計，再包裝該連線。新的勝者只用於新請求。一般轉發的 TCP/443 在獨立錯誤或目標失敗後可以調整後續選線，已經建立的其他連線會繼續傳輸。手動封鎖及影音的專用劣化處置仍可關閉連線。其他協定和內部連線沿用既有品質處置。[整合程式](../../adapter/outboundgroup/smart.go)

同步時同時核對 TCP／UDP 成功、勝者發佈、品質統計及上游可能新增的連線清理路徑。驗收需涵蓋 A 已有長連線、B 承接同一目標的新連線，再觀察 A 客戶端是否完整收到資料。

## SC-02 相同 target 共用選舉結果

**問題與目的：** 同 target 的並發請求各自刷新／競速，可能互相覆蓋 winner；未成功撥號的背景排名結果也可能取代正在使用的節點。需要把「計算候選」與「發佈已成功的 winner」分開。

**必須保留：**

- 需要重新選路的請求以設定、群組及 winner 快取的 target key 共用一輪選舉。一般流量使用 `SmartTarget`，影片 CDN 在同一 `SmartTarget` 下按 TCP 和 UDP 各自選舉。ASN 差異不拆開同一輪選舉。
- 一輪中只有 leader 以成功撥號結果發佈新的 winner。follower 使用可用的舊結果或等待 leader；不得在完成自己的撥號後覆蓋新 winner。
- 不再由過期 cache 的背景排名刷新直接寫入候選列表，也不因任意一次撥號失敗就無條件清除共用 unwrap cache。
- leader 失敗、等待取消、winner 缺失或 winner 已不在候選 provider 中，都須向呼叫者返回明確錯誤；不任意替換成未經本輪選擇的節點。

| 狀態 | 行為 |
| --- | --- |
| 手動選定的成員仍在分組 | 直接使用固定成員 |
| 快取勝者仍合格，閒置期限和重選條件均允許使用 | 撥該勝者，成功後只續同一版勝者的閒置期限 |
| 同目標需要重選 | 由一名 leader 使用現有排名及候選篩選。成功後發佈新的勝者 |
| 已有 leader，舊勝者仍合格 | 並行請求可以撥舊勝者，續期時需比對原選舉結果 |
| 已有 leader，沒有合格的舊勝者 | 按呼叫 context 等待，從本次 provider 快照查找已發佈的勝者 |

TCP 自動分組設定 `policy-priority` 時，以選定時間計算下次重選。使用分組健康檢查的 `interval`，缺省沿用五分鐘的 Smart 排名週期。到期且有較高倍率的合格成員，或原先不合格的高倍率成員恢復可用，會在下一條自然請求啟動重選。既有 URL 測試歷史也可辨識兩次請求之間發生的探測失敗與恢復。沿用現有候選上限 10 個，為各可用倍率層及仍在合格排名內的舊勝者保留位置。各層按倍率取得短暫先發，層內沿用選出時的排名。UDP 共用勝者協調並維持自己的順序嘗試。

快取保留選定時間，即使持續命中。ASN 別名沿用原選舉與閒置期限，過期別名會等待新勝者。實際傳輸撥號失敗可讓原選舉失效，晚到的成功或失敗均需比對原選舉，藉此保護已發布的新結果。一般流量共用 TCP 與 UDP 的 winner，使用前依實際協定和 provider 快照再檢查。[選舉協調](../../adapter/outboundgroup/smart_selection.go) · [勝者記憶體](../../component/smart/memory.go)

測試涵蓋候選上限與倍率層、回升及定時重選條件、64 個併發呼叫協調、失敗傳遞、等待取消，以及舊撥號和 ASN 別名的續期限制。同步時核對 cache key、選定時間、寫入時機與 TCP／UDP 入口。[選舉測試](../../adapter/outboundgroup/smart_selection_test.go) · [快取測試](../../component/smart/unwrap_cache_test.go)

## SC-03 避免把取消與端點政策當成節點故障

- TCP 的全部候選共用外層請求期限，Smart 預設上限為 `DefaultTCPTimeout`。較高倍率層的先發時間沿用目標握手估計的五倍，缺少樣本時使用最近一次分組 URL 測試延遲。全部先發窗口最多共用剩餘期限的一半，其餘時間留給後續候選。到時啟動下一層，已發出的撥號可繼續完成。發出應用資料的只有採用的連線。
- 記錄個別撥號的成功、真實傳輸失敗、競速取消、外層截止、嘗試截止及其他請求取消。只有真實傳輸失敗寫入節點統計。所有候選在期限內明確失敗時保留各自的錯誤。
- `StatusTest` 探測 host 根路徑。沒有 transport error 且狀態為 403 或 405 時，與原先成功狀態同樣計入可達。503 等其他 `ok=false` 狀態仍是異常。品質與恢復檢查共用該判定，原始 HTTP 請求由端點處理。

先發和取消分類適用於 TCP 撥號。UDP 的撥號期限與統計入口維持既有設計。已建立的 TLS 連線上的應用狀態，只能由應用端或與傳輸相關的獨立訊號判斷。[撥號](../../adapter/outboundgroup/smart_dial.go) · [回歸測試](../../adapter/outboundgroup/smart_dial_test.go)

## SC-04 持續劣化監測

**目的：** 在保留正常連線的同時，為已建立但持續低吞吐的連線提供可解釋、有範圍的處理機制。一次慢回應或多條同時等待的連線不足以確認節點劣化。

**納入範圍**

監測適用於目的埠 443、有 `WildcardTarget` 的 TCP 連線，以及 `googlevideo.com` 及其子網域的 UDP 連線。兩者都從既有 tracker 取流量差分，排除 `INNER` 連線。

**目前參數與判定：**

| 參數 | 值／用途 |
| --- | --- |
| 採樣間隔 | tunnel Running 後，每 1 秒取樣 |
| 有意義的上傳 | 觀測區段內至少 128 bytes |
| 零回應訊號 | 經過至少 5 秒，下載仍為 0；只輸出警告，不進入自動處置 |
| 持續低吞吐訊號 | 下載至少 16 KiB、區段至少 2.5 秒，平均下載速率低於 16 KiB/s |
| 新區段分隔 | 有新上傳、之前已有下載且距上次觀測下載至少 500 ms 時，可開始新區段；這是流量啟發式，不是 HTTP request 邊界 |
| 確認後冷卻 | 5 分鐘 |

一般 TCP 在一個觀測區段內最多發出一次訊號。區段達到檢查門檻且速率健康時，結束該區段。

一般 TCP 的低吞吐是診斷訊號。有效但低流量的連線保留，快取和冷卻狀態不因此改變。正常低流量與無流量關閉可影響品質排序，單靠這些資料不建立目標故障紀錄。零回應只輸出警告。

YouTube CDN 的 TCP 和 UDP 連線只要連續採樣至少 2.5 秒、每一段都有回流但速率低於 16 KiB/s，且累計上傳至少 128 bytes、下載至少 16 KiB，即可在同一條連線確認劣化。暫停下載、完全無回包或恢復正常速率都會結束該觀測區段。手動固定節點時略過影片劣化處置。

**確認後的行為**

1. 冷卻以節點、wildcard 和傳輸協定為單位，期限為 5 分鐘。
2. 影片 CDN 的 winner 快取和選舉按協定隔離，停用 ASN 共用。確認劣化時清除該協定的 winner，並阻止冷卻節點寫回。
3. 只關閉被確認劣化的 tracker。tracker 以原子旗標保留關閉原因，Smart 關閉統計可辨識這是核心主動關閉。
4. `filterProxies` 排除冷卻中的候選，影片 UDP 只使用支援 UDP 的節點。候選用盡時由選路入口返回錯誤。
5. Smart 群組的 `videoDegradations` 會列出節點、協定、最近確認時間和冷卻到期時間。localClash 的增強篩選可在每輪開始時讀取這些資料，略過仍在冷卻或自上一輪取樣後確認劣化的出口。

**必須保留的效能／生命週期邊界：** 不攔截每次 Read／Write 重新計量；從既有 tracker 取樣。`ReaderReplaceable`／`WriterReplaceable`／`Upstream` 允許 relay 解包，避免破壞底層傳輸快路徑。monitor 等待 tunnel Running，隨 Smart context 取消而結束；Close 將連線移出監測集合。

**實作限制及同步注意：**

- tracker 以 SmartTarget 索引加上 metadata 指標相等來定位；上游若複製 metadata、改變 wrapper／tracker 建立順序或計量位置，需要重新適配。
- 冷卻及證據位於 Smart 實例記憶體中，沒有跨實例／重啟持久化。
- 低吞吐是流量與時間的啟發式證據，不能證明節點是慢速的唯一根因；例如應用本身也可能刻意限速。
- 手動選定節點的路徑會提前返回，跳過 `filterProxies` 的冷卻排除。一般 TCP 的監測沿用原有適用範圍。
- cache 已清除後才再定位 tracker；找不到 tracker 或 Close 失敗時會記錄警告，冷卻不會自動回滾。
- monitor 直接關閉的是確認連線；SC-01 所述原有 force-close 路徑仍存在，不能把整個 Smart 的關閉範圍描述成只限此連線。

**驗證**

[monitor tests](../../adapter/outboundgroup/smart_active_monitor_test.go) 涵蓋 relay 解包、一般 TCP 低吞吐時保留 tracker 及勝者、影片間歇下載、影片 TCP 和 UDP tracker 關閉、冷卻節點無法寫回快取、下一條 UDP 連線重選和 TCP winner 隔離。實際 YouTube 播放緩衝和長時間使用待路由器驗證。

## SC-05 分階段診斷

**目的：** 將 proxy 建連耗時與建連後的首次寫入／讀取分開，讓延遲調查有同一條連線的可關聯證據；診斷時間本身不新增選路、評分或關閉決策。

**目前實作：** [smart_observability.go](../../adapter/outboundgroup/smart_observability.go) 在 TCP metric wrapper 中建立 trace，輸出 `[SmartTiming]` debug 記錄；包含 group、node、target、wildcard、proxy-connect、stream-ready-elapsed、first-write-to-read 及錯誤。關閉記錄加入 tracker ID、連線存續時間及流量；既有 `[Smart] Connection status` 也加入 metadata UUID。

開啟 debug 後，選舉事件以同一個 dial ID 記錄倍率、加權分數、排除原因、候選順位、快取年齡、各次撥號結果和後續層啟動原因。可用 ID 對照同一輪的候選名單與實際嘗試。

**必須保留：**

- proxy-connect、wrapper 就緒後的經過時間、首次寫到首次讀的間隔不能互相冒充。
- 只有觀測過 first-write，才能計算 first-write-to-read；缺失時標記 `unobserved`，不能填 0 或以 proxy-connect 代替。
- trace 關聯與錯誤值必須可用，但不改變既有 latency metric 的意義。

**邊界：** first-write callback 只在 `N.NeedHandshake(c)` 為真時安裝；不是每條 TCP 連線都有 first-write 記錄。這些時間是傳輸層 callback 的觀測，不是 DNS、TLS、HTTP TTFB 或頁面完成時間的完整分解。關閉記錄依賴找到 tracker；UDP 沒有這組 timing 事件。

**驗證：** [observability tests](../../adapter/outboundgroup/smart_observability_test.go) 覆蓋階段差值、first-write 缺失明確標記與錯誤值。程式以 CompareAndSwap 保留首次寫入時間，但目前沒有重複 first-write 的針對性測試；其他缺口包括完整 wrapper callback、真實連線 debug log 及所有 relay 解包路徑。

## SC-06 TCP DoH 查詢內切換備援

TCP DoH 的 TLS 或 DNS 回應可能在 proxy CONNECT 已成功後卡住。這時 Smart 一般 TCP 路徑已完成撥號選擇，DoH 仍需保留時間完成備援的 CONNECT、TLS 與 DNS 交換。

- 適用於明確指定 Smart 或經 `select` 鏈選到自動 Smart 的 HTTPS HTTP/1.1 與 HTTP/2 上游。固定 Smart 成員、其他分組類型及 HTTP/3 沿用各自的選路。
- 主請求耗用查詢期限的一半後，若已取得 TCP 連線或使用既有 HTTP 連線且尚未得到結果，啟動一次同上游的備援交換。兩路共用原查詢期限，先取得可用 DNS 回應的一路完成查詢。原請求可在備援等待期間繼續接收回應。
- Smart 用現有排序和並行撥號選出備援。排序以同組的健康候選為輸入，排除本次卡住的成員與內建直連／拒絕節點，遵守組內封鎖和冷卻。
- 備援有效回覆可替換該 DoH 上游的 HTTP client，讓後續查詢重用新連線。替換須確認原 client 和選路仍是當前版本。舊 transport 的活動請求可繼續完成。一般 TCP／UDP 的 Smart winner 仍由 SC-02 的共用選舉管理。
- 這次無回應只影響該筆查詢的備援選擇。失敗或取消保留原查詢期限和錯誤，節點品質監測仍依 SC-03、SC-04 的條件處理。

[DoH 交換](../../dns/doh_smart.go)以同一 retry ID 記錄開始備援、Smart 完成排序、備援撥號完成與收到有效 DoH。現有 [Smart 連線診斷](#sc-05-分階段診斷)仍以 TCP tracker 為範圍。備援撥號及分組銜接見 [Smart 備援](../../adapter/outboundgroup/smart_doh.go) 和 [DNS 路由](../../tunnel/dns_smart.go)。

2026-11-11 在 macOS arm64 上以相同 50 候選及 HTTP/2 測試端點重跑三輪。已快取出口的 TLS 靜默、重用連線的 DoH 靜默及冷連線 TLS 靜默，各 3/3 筆均在約 2.54 秒收到同一查詢的備援答案。另驗證固定成員、兩路靜默、原請求較慢但可成功，以及備援成功時另一筆並行請求仍可在原連線完成。端點與延遲為 loopback 模擬，正式出口上的表現待實機驗證。

## SC-07 LightGBM 啟動更新與排程重試

啟動時若現有模型已超過更新週期，Smart 會立即嘗試下載。下載失敗時保留已載入的模型與磁碟上的模型檔案，記錄原始錯誤，並在下一個排程週期向同一來源重試。下載成功且通過驗證後才寫入並重新載入模型。[LightGBM 更新程式](../../component/updater/update_lgbm.go)

回歸測試讓本機 HTTP 端點在啟動下載時傳回不完整的模型，驗證下一個排程週期仍會送出請求，且舊模型檔案保留到新模型驗證成功。[更新回歸測試](../../component/updater/update_lgbm_test.go)

## 驗證入口

### 2026-11-15 Smart 先發、回升與串流保留

在拋棄式 iStoreOS 24.10.8 QEMU guest 跑 Linux amd64 核心 `94924df9…f3528544`，用外部客戶端和受控代理驗證兩次低流量 SSE 均完整、歷史握手很短的單節點在增至約一秒後仍可用、較慢的高倍率在先發窗口內勝出、恢復的高倍率承接新請求時原低倍率長串流仍完整，以及重複成員的三層選路。另以持續上傳和低速下載的受控 TLS 串流驗證影片 TCP 確認及 tracker 關閉。單元測試覆蓋候選上限、影片 TCP 冷卻與 UDP 重選。正式路由器、真實 ChatGPT 及影片實播仍待驗證。

### 2026-09-22 UDP 回退記錄

撤銷 `92844c60`，恢復 `651ca468` 的 Smart 程式行為。使用者觀察到出口疑似過早被放棄、切換頻繁；程式審查發現該改動延後共用 TCP／UDP 選舉完成時間，且單條 UDP/443 在上傳至少 2400 bytes、5 秒無回包時便冷卻 node＋wildcard 5 分鐘，並無條件刪除共用 target／ASN 快取。這些是程式層面的退化路徑，尚未以現場日誌證明是體感問題的實際原因。

本次完整撤回該改動，不加入新的門檻或替代機制：UDP association 建立成功後即完成選舉，首次回包仍由原有 callback 記錄 latency；不再執行此次新增的 UDP 無回包冷卻／清快取／關閉。既有 TCP 持續劣化監測及其他原有品質處置保留。

本機 Smart 專項及 race 通過；`CGO_ENABLED=0 SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1 go test ./... -count=1` 與同命令加 `-tags with_gvisor` 均通過。完整套件測試沿用 CI 的並發／interop 跳過設定，未執行那些被跳過的情境。跨平台分發另由 release workflow 驗證；真實網路改善仍待更新後由使用者觀察。回退不保證所有出口切換都消失，也不把 GitHub 發佈成功當作路由器已更新的證據。

```bash
go test ./adapter/outboundgroup -run 'TestSmart|TestShouldRecordSmartDialFailure' -count=1
CGO_ENABLED=1 go test -race ./adapter/outboundgroup -run 'TestSmart|TestShouldRecordSmartDialFailure' -count=1
CGO_ENABLED=0 SKIP_INTEROP_TEST=1 go test ./... -count=1
CGO_ENABLED=0 SKIP_INTEROP_TEST=1 go test ./... -tags with_gvisor -count=1
```

2026-09-03 整理本文件時，亦以本機 Go 1.26.0／darwin arm64 執行上述 Smart 專項 `-race` 命令，結果通過；沒有修改程式或新增測試。

同日的[固定提交 CI](https://github.com/qoli/mihomo-Alpha/actions/runs/33698437226)已通過包含 Go 1.26、1.23、1.20 的 Build 矩陣測試路徑。同步時若只改文檔，可引用這份固定提交證據並核對程式未變；若改選路／cache／tracker，應依上面的缺口補充針對性驗證。
