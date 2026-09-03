# Smart 連線行為意圖

狀態：已實作，核對至 `02c0eaf8`；共同基準、提交及驗證成熟度見[索引](README.md)。

本文件區分需要保留的行為、目前實作方法與尚未證明的部分。常數和函式可以隨上游重構改變；改變判定門檻或影響範圍時，必須同步更新意圖及證據。

## SC-01 成功撥號不淘汰既有連線

**問題與目的：** 上游會在 TCP／UDP 撥號成功後呼叫 `closeSameConnection(..., false)`，關閉同 SmartTarget、同組內不經新 winner 的其他連線。新連線競速勝出本身並不能證明舊連線故障；不應因此中斷既有串流。

**必須保留：** TCP `DialContext` 與 UDP `ListenPacketContext` 的成功路徑，不得只因新 winner 不同就淘汰其他已建立的連線。winner 記錄、成功統計及 metric wrapper 仍須正常完成。

**目前實作：** 從這兩個成功路徑移除非強制 `closeSameConnection` 呼叫。[整合程式](../../adapter/outboundgroup/smart.go)仍保留該函式及真正故障／劣化的處理。

**邊界：** 這不是「永不關閉既有連線」。`recordConnectionStats` 仍可在 `isDegraded || failedBlock` 時呼叫 `closeSameConnection(..., true)`；其範圍是同 SmartTarget 且 chains 含此組的其他 tracker，並不限於同一 proxy。客戶端關閉、連線錯誤及 SC-04 的確認劣化也仍可造成關閉。

**同步檢查：** 同時檢查 TCP／UDP 成功路徑、winner 發佈路徑及上游新引入的 eviction／cleanup hook。不能只搜尋舊函式名稱。

**驗證缺口：** 目前沒有直接覆蓋「A 節點舊連線保持開啟，B 節點新撥號成功」的 tracker 整合測試。同步相關路徑時應驗證此情境，並另確認真實故障仍可依原有規則關閉連線。

## SC-02 相同 target 共用選舉結果

**問題與目的：** 同 target 的並發請求各自刷新／競速，可能互相覆蓋 winner；未成功撥號的背景排名結果也可能取代正在使用的節點。需要把「計算候選」與「發佈已成功的 winner」分開。

**必須保留：**

- 對需要重新選路的請求，以 `configName + groupName + SmartTarget` 協調同一輪選舉。這必須對齊 `StoreUnwrapResult` 的 target 級 cache 身分；不能因 ASN 不同而為同一 target 建立互相競爭的選舉。
- 一輪中只有 leader 以成功撥號結果發佈新的 winner。follower 使用可用的舊結果或等待 leader；不得在完成自己的撥號後覆蓋新 winner。
- 不再由過期 cache 的背景排名刷新直接寫入候選列表，也不因任意一次撥號失敗就無條件清除共用 unwrap cache。
- leader 失敗、等待取消、winner 缺失或 winner 已不在候選 provider 中，都須向呼叫者返回明確錯誤；不任意替換成未經本輪選擇的節點。

**目前實作：** [smart_selection.go](../../adapter/outboundgroup/smart_selection.go) 的 `smartSelectionCoordinator`、`smartSelectionFlightKey`，以及 `smart.go` 的 `prepareDialSelection`／`finishDialSelection`：

| 狀態 | 行為 |
| --- | --- |
| 明確手動選定節點 | 直接使用，不進入選舉 |
| 未過期 cache 且篩選後只有一個候選 | 直接使用，成功可記錄 winner |
| 需要重選且成為 leader | 跳過 unwrap cache 重新計算候選，成功才寫 cache 並喚醒等待者 |
| 已有 leader，且本請求取得 cache 與非空候選 | 使用篩選結果的第一個候選，`persistWinner=false` |
| 已有 leader，且沒有可立即使用的 cache 結果 | 受 context 約束地等待，按 winner 名稱在本次 provider 快照中查找 |

TCP leader 第一批競速完整的有界候選集，目前 `maxSelected=10`，不讓所有等待者承受第一個節點逾時後才開始下一批。一般非 leader 的批次方法為首個節點，再以 `parallelDials=5` 分批，受 `maxRetries=3` 限制。UDP 共用同一 coordinator，但保留自身的順序嘗試／重試方式，不宣稱與 TCP 有相同競速方式。

**實作限制：** key 沒有 ASN，也沒有 TCP／UDP 維度；跨協議使用 winner 的 provider 能力、provider 更新期間的快照差異，需要人工檢查。follower 的「舊 winner」實際是篩選後的第一個候選；cache 名稱已失效或被過濾時，不保證仍是原節點。協調器不是永久鎖：一輪 finish 後移除 flight，後續必要時可以開始新一輪。

**驗證：** [selection tests](../../adapter/outboundgroup/smart_selection_test.go) 覆蓋 key、TCP 批次、64 個並發呼叫共用 winner、錯誤傳遞、缺失 winner 及等待取消。這些是 helper／coordinator 測試；沒有涵蓋完整的 `prepareDialSelection`＋真實 TCP／UDP 撥號、過期 cache 競爭或 provider 熱更新。

**同步檢查：** 必須一起核對 `component/smart/memory.go` 的 unwrap cache 身分與寫入時機、`filterProxies` 的候選補足規則、TCP／UDP 成功與失敗路徑。上游若更改 cache 維度，不能只保留舊 coordinator key。

## SC-03 避免把取消與端點政策當成節點故障

**必須保留的兩項區分：**

1. TCP 競速取消後，落敗嘗試回傳的錯誤不能污染節點失敗統計。`shouldRecordSmartDialFailure` 只在 `err != nil`、錯誤不是 `context.Canceled` 且 `ctx.Err()==nil` 時允許記錄。
2. `StatusTest` 探測的是 host 根路徑，未必是使用者請求的 URL。沒有 transport error 時，原判定成功或 HTTP 403／405 都視為這條探測路徑可達；其他 `ok=false` 狀態，例如 503，仍然異常。

HTTP 政策判定同時用於 `checkNodeQuality` 與 `checkHostStatus` 的恢復檢查，避免一端放行、另一端持續封鎖。它不代表使用者的原始請求已成功，也不改寫網站的 HTTP 回應。

**目前邊界：** 取消分類 helper 只接在 TCP `singleDialContext`；當 context 已 deadline exceeded 時也不記錄該次 TCP 失敗。不能將這項保證擴大成 UDP 或所有統計入口都已排除取消。HTTP 403／405 例外仍以呼叫端 `err == nil` 為前提。

**驗證：** `TestShouldRecordSmartDialFailureIgnoresCanceledRaceLoser`、`TestSmartStatusProbeAcceptedTreatsEndpointPolicyAsReachable`。尚缺 TCP／UDP 完整統計鏈與真實根路徑探測的整合驗證。

## SC-04 持續劣化監測

**目的：** 在保留正常連線的同時，為已建立但持續低吞吐的連線提供可解釋、有範圍的處理機制。一次慢回應或多條同時等待的連線不足以確認節點劣化。

**納入範圍：** `monitorActiveConnection` 只包裝 TCP、目的 port 443、非 `INNER`、且有 `WildcardTarget` 的連線；不涵蓋 UDP／QUIC、其他 port 或所有 HTTPS 應用層請求。它使用 tracker 累計流量差分，沒有解析 HTTP。

**目前參數與判定：**

| 參數 | 值／用途 |
| --- | --- |
| 採樣間隔 | tunnel Running 後，每 1 秒取樣 |
| 有意義的上傳 | 觀測區段內至少 128 bytes |
| 零回應訊號 | 經過至少 5 秒，下載仍為 0；只輸出警告，不進入自動處置 |
| 持續低吞吐訊號 | 下載至少 16 KiB、區段至少 2.5 秒，平均下載速率低於 16 KiB/s |
| 新區段分隔 | 有新上傳、之前已有下載且距上次觀測下載至少 500 ms 時，可開始新區段；這是流量啟發式，不是 HTTP request 邊界 |
| 確認視窗 | 2 分鐘 |
| 確認後冷卻 | 5 分鐘 |

每個觀測區段最多發出一次訊號；達到低吞吐檢查門檻但速率健康時，結束該區段的觀測。

**自動處置的必要條件：** 同一 Smart 實例內，相同 `proxyName + WildcardTarget` 在 2 分鐘內有第二條不同連線也發出持續低吞吐訊號，且第一條被觀察的連線已關閉。以下情況不能確認：同一連線重複觀測、前一條連線仍開啟、僅零回應、證據超時，或已在冷卻期內。

**確認後的目前行為：**

1. 為 node＋wildcard 記錄冷卻；不同 node 或不同 wildcard 不共享此冷卻。
2. 清除確認連線的 target 級 unwrap cache。呼叫傳入空 ASN，不清除 ASN 級 cache。
3. 找到該連線 tracker，設定 `SmartBlock="degraded"` 後關閉它。此標記讓既有品質檢查避免將強制關閉再當作新劣化。
4. `filterProxies` 的排名、補足及既有兜底遍歷都排除冷卻中的 node＋wildcard。若因此沒有候選，由選路入口返回明確錯誤。

**必須保留的效能／生命週期邊界：** 不攔截每次 Read／Write 重新計量；從既有 tracker 取樣。`ReaderReplaceable`／`WriterReplaceable`／`Upstream` 允許 relay 解包，避免破壞底層傳輸快路徑。monitor 等待 tunnel Running，隨 Smart context 取消而結束；Close 將連線移出監測集合。

**實作限制及同步注意：**

- tracker 以 SmartTarget 索引加上 metadata 指標相等來定位；上游若複製 metadata、改變 wrapper／tracker 建立順序或計量位置，需要重新適配。
- 冷卻及證據位於 Smart 實例記憶體中，沒有跨實例／重啟持久化。
- 低吞吐是流量與時間的啟發式證據，不能證明節點是慢速的唯一根因；例如應用本身也可能刻意限速。
- 手動選定節點的路徑會提前返回，跳過 `filterProxies` 的冷卻排除；monitor 包裝本身沒有排除手動模式。不能宣稱冷卻對手動選路也有相同效果。
- cache 已清除後才再定位 tracker；找不到 tracker 或 Close 失敗時會記錄警告，冷卻不會自動回滾。
- monitor 直接關閉的是確認連線；SC-01 所述原有 force-close 路徑仍存在，不能把整個 Smart 的關閉範圍描述成只限此連線。

**驗證：** [monitor tests](../../adapter/outboundgroup/smart_active_monitor_test.go) 覆蓋 relay unwrap、微量上傳不觸發、健康／低吞吐區分、同連線不能重複確認、零回應不確認、冷卻範圍與到期。現有「證據過期」用例使用同一連線與零回應原因，即使移除視窗限制也可能通過；有效低吞吐證據跨視窗失效，仍缺針對性驗證。其他缺口包括完整 tracker 搜尋／關閉、cache 刪除、所有候選遍歷和手動模式的整合效果；不能由這些單元測試推導實際 Safari 頁面已改善。

## SC-05 分階段診斷

**目的：** 將 proxy 建連耗時與建連後的首次寫入／讀取分開，讓延遲調查有同一條連線的可關聯證據；診斷時間本身不新增選路、評分或關閉決策。

**目前實作：** [smart_observability.go](../../adapter/outboundgroup/smart_observability.go) 在 TCP metric wrapper 中建立 trace，輸出 `[SmartTiming]` debug 記錄；包含 group、node、target、wildcard、proxy-connect、stream-ready-elapsed、first-write-to-read 及錯誤。關閉記錄加入 tracker ID、連線存續時間及流量；既有 `[Smart] Connection status` 也加入 metadata UUID。

**必須保留：**

- proxy-connect、wrapper 就緒後的經過時間、首次寫到首次讀的間隔不能互相冒充。
- 只有觀測過 first-write，才能計算 first-write-to-read；缺失時標記 `unobserved`，不能填 0 或以 proxy-connect 代替。
- trace 關聯與錯誤值必須可用，但不改變既有 latency metric 的意義。

**邊界：** first-write callback 只在 `N.NeedHandshake(c)` 為真時安裝；不是每條 TCP 連線都有 first-write 記錄。這些時間是傳輸層 callback 的觀測，不是 DNS、TLS、HTTP TTFB 或頁面完成時間的完整分解。關閉記錄依賴找到 tracker；UDP 沒有這組 timing 事件。

**驗證：** [observability tests](../../adapter/outboundgroup/smart_observability_test.go) 覆蓋階段差值、first-write 缺失明確標記與錯誤值。程式以 CompareAndSwap 保留首次寫入時間，但目前沒有重複 first-write 的針對性測試；其他缺口包括完整 wrapper callback、真實連線 debug log 及所有 relay 解包路徑。

## 驗證入口

```bash
go test ./adapter/outboundgroup -run 'TestSmart|TestShouldRecordSmartDialFailure' -count=1
CGO_ENABLED=1 go test -race ./adapter/outboundgroup -run 'TestSmart|TestShouldRecordSmartDialFailure' -count=1
CGO_ENABLED=0 SKIP_INTEROP_TEST=1 go test ./... -count=1
CGO_ENABLED=0 SKIP_INTEROP_TEST=1 go test ./... -tags with_gvisor -count=1
```

2026-09-03 整理本文件時，亦以本機 Go 1.26.0／darwin arm64 執行上述 Smart 專項 `-race` 命令，結果通過；沒有修改程式或新增測試。

同日的[固定提交 CI](https://github.com/qoli/mihomo-Alpha/actions/runs/33698437226)已通過包含 Go 1.26、1.23、1.20 的 Build 矩陣測試路徑。同步時若只改文檔，可引用這份固定提交證據並核對程式未變；若改選路／cache／tracker，應依上面的缺口補充針對性驗證。
