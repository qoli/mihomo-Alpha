# Smart 連線行為意圖

狀態為已實作。2026-11-26 更新 SC-04 的 UDP/443 無回包否決；2026-11-25 更新 SC-02 的進行中選舉地址家族隔離；2026-11-18 更新 SC-02 和 SC-03 的 UDP 撥號期限與錯誤分類。共同基準、歷史提交與驗證成熟度見[索引](README.md)。

本文件區分需要保留的行為、目前實作方法與尚未證明的部分。常數和函式可以隨上游重構改變；改變判定門檻或影響範圍時，必須同步更新意圖及證據。

## SC-01 成功撥號不淘汰既有連線

**問題與目的：** 上游會在 TCP／UDP 撥號成功後呼叫 `closeSameConnection(..., false)`，關閉同 SmartTarget、同組內不經新 winner 的其他連線。新連線競速勝出本身並不能證明舊連線故障；不應因此中斷既有串流。

**必須保留：** TCP `DialContext` 與 UDP `ListenPacketContext` 的成功路徑，不得只因新 winner 不同就淘汰其他已建立的連線。winner 記錄、成功統計及 metric wrapper 仍須正常完成。

TCP 與 UDP 成功後會更新勝者及連線統計，再包裝該連線。新的勝者只用於新請求。一般轉發的 TCP/443 在獨立錯誤或目標失敗後可以調整後續選線，已經建立的其他連線會繼續傳輸。手動封鎖及影音的專用劣化處置仍可關閉連線。其他協定和內部連線沿用既有品質處置。[整合程式](../../adapter/outboundgroup/smart.go)

同步時同時核對 TCP／UDP 成功、勝者發佈、品質統計及上游可能新增的連線清理路徑。驗收需涵蓋 A 已有長連線、B 承接同一目標的新連線，再觀察 A 客戶端是否完整收到資料。

## SC-02 相同 target 共用選舉結果

**問題與目的：** 同 target 的並發請求各自刷新／競速，可能互相覆蓋 winner；未成功撥號的背景排名結果也可能取代正在使用的節點。需要把「計算候選」與「發佈已成功的 winner」分開。

**必須保留：**

- 需要重新選路的請求以設定、群組、winner 快取的 target key、傳輸協定及目前撥號的目的地址類型共用一輪進行中選舉。一般流量的 target key 是 `SmartTarget`，影片 CDN 沿用按協定區分的 target key。ASN、來源地址及同家族內的目的 IP／埠差異不拆開同一輪選舉。
- 目的地址類型依傳入撥號 metadata 的 `AddrType()` 區分 IPv4、IPv6 與主機名。IP 撥號中的 IPv4-mapped IPv6 歸入 IPv4；保留 `Host` 的請求沿用主機名語意，即使同時有規則解析的 `DstIP`。`DNSMapping`／`DNSHosts` 的入口已透過 `Pure()` 清除 Host 時，依該次實際使用的 IP 分類。選舉協調不另行解析 DNS。
- 一輪中只有 leader 以成功撥號結果發佈新的 winner。follower 使用可用的舊結果或等待 leader；不得在完成自己的撥號後覆蓋新 winner。
- 不再由過期 cache 的背景排名刷新直接寫入候選列表，也不因任意一次撥號失敗就無條件清除共用 unwrap cache。
- leader 失敗、等待取消、winner 缺失或 winner 已不在候選 provider 中，都須向呼叫者返回明確錯誤；不任意替換成未經本輪選擇的節點。

| 狀態 | 行為 |
| --- | --- |
| 手動選定的成員仍在分組 | 直接使用固定成員 |
| 快取勝者仍合格，閒置期限和重選條件均允許使用 | 撥該勝者，成功後只續同一版勝者的閒置期限 |
| 同目標、同協定、同目的地址類型需要重選 | 由一名 leader 使用現有排名及候選篩選。成功後發佈新的勝者 |
| 已有相同等待鍵的 leader，舊勝者仍合格 | 並行請求可以撥舊勝者，續期時需比對原選舉結果 |
| 已有相同等待鍵的 leader，沒有合格的舊勝者 | 按呼叫 context 等待，從本次 provider 快照查找已發佈的勝者 |

TCP 自動分組設定 `policy-priority` 時，以選定時間計算下次重選。使用分組健康檢查的 `interval`，缺省沿用五分鐘的 Smart 排名週期。到期且有較高倍率的合格成員，或原先不合格的高倍率成員恢復可用，會在下一條自然請求啟動重選。既有 URL 測試歷史也可辨識兩次請求之間發生的探測失敗與恢復。沿用現有候選上限 10 個，為各可用倍率層及仍在合格排名內的舊勝者保留位置。各層按倍率取得短暫先發，層內沿用選出時的排名。

UDP 依排名順序建立 association。有請求期限且還有後續候選時，單次嘗試同時受既有撥號期限和剩餘時間均分結果限制。第一個候選即使缺少連線時間樣本，後續候選也能取得撥號時間。只有單一候選時沿用原本的單次期限與重試次數上限，請求截止後停止嘗試。association 成功即可發佈勝者。

快取保留選定時間，即使持續命中。ASN 別名沿用原選舉與閒置期限，過期別名會等待新勝者。實際傳輸撥號失敗可讓原選舉失效，晚到的成功或失敗均需比對原選舉，藉此保護已發布的新結果。地址家族隔離的範圍限於進行中的等待鍵；winner 快取、排名及 ASN 共用沿用既有範圍。一般流量的 TCP 與 UDP 仍共用 winner 快取，使用前依實際協定和 provider 快照再檢查。[選舉協調](../../adapter/outboundgroup/smart_selection.go) · [勝者記憶體](../../component/smart/memory.go)

自動選路在放寬目標失敗或 URL 健康條件後仍須檢查 UDP 能力。候選用盡時，封包入口回報選路錯誤。UDP follower 仍會確認其 leader 發佈的節點支援 UDP。

UDP 無回包否決是額外的協定專屬資格檢查：即使 winner 快取仍在，或候選耗盡而進入放寬分支，也不得沿用仍在冷卻的 node＋wildcard UDP 出口。follower 取得 winner 後與 UDP 撥號返回後同樣檢查；撥號途中才被否決的新 association 在交付封包前釋放並結束該候選的嘗試。否決中的 UDP 出口不能寫回或續期 winner。這些檢查不延後選舉至首個回包，也不關閉已交付的 association。

測試涵蓋候選上限與倍率層、回升及定時重選條件、64 個併發呼叫協調、失敗傳遞、等待取消，以及舊撥號和 ASN 別名的續期限制。UDP 回歸涵蓋非影音目標的備選、候選用盡、手動指定、同一目標的 TCP／UDP 各自選舉，以及首個 UDP 候選逾時後，下一個合格候選成功並讓同協定等待者取得勝者。同步時核對 cache key、選定時間、寫入時機與 TCP／UDP 入口。[選舉測試](../../adapter/outboundgroup/smart_selection_test.go) · [快取測試](../../component/smart/unwrap_cache_test.go)

### 2026-11-25 跨地址家族等待回歸

在未修改產品程式的 `a1c312edc5896b944a85bb30ccf684177bb1ba70` 上先加入[地址家族回歸測試](../../adapter/outboundgroup/smart_selection_family_test.go)。同一 Smart 群組、同一 `GeoIP [telegram]` 的 IPv6 TCP leader 等待 loopback HTTP CONNECT 回覆時，IPv4 請求沒有向代理送出 CONNECT，隨後繼承 `smart selection wait failed for GeoIP [telegram]: 502 Could not resolve host [2001:b28:f23d:f001::a]`，測試失敗。這重現了 11 月 24 日現場紀錄的錯誤傳遞；11 月 25 日 01:07 的現場採樣未再出現相同錯誤。

修正等待鍵後，同一測試確認 IPv6 leader 尚未完成時，`149.154.175.50:5222` 與 `149.154.175.58:5222` 分別送出自己的 CONNECT 並完成 echo 資料交換，IPv6 隨後仍返回原始 502。另以 TCP／UDP 共 24 個情境驗證同家族等待、不同家族獨立、IPv4-mapped IPv6、主機名優先及 `Pure()` 後的地址語意；既有 TCP／UDP 獨立選舉測試亦保留。

2026-11-25 在 Go 1.26.0、darwin/arm64 的目前 Alpha checkout 執行：

```bash
# 修正前失敗，修正後通過
go test ./adapter/outboundgroup -run '^TestSmartConcurrentSelection(SeparatesDestinationFamilies|DestinationScope)$' -count=1 -v
# 修正後整個 outboundgroup 套件及競態檢查均通過
go test ./adapter/outboundgroup -count=1
CGO_ENABLED=1 go test -race ./adapter/outboundgroup -count=1
```

驗證範圍為本機選舉協調、受控代理的實際 TCP CONNECT 與資料交換，以及 outboundgroup 套件的 race 檢查。尚未部署到路由器，也未驗證真實 Telegram 或修復代理端的 IPv6 502。

## SC-03 避免把取消與端點政策當成節點故障

- TCP 的全部候選共用外層請求期限，Smart 預設上限為 `DefaultTCPTimeout`。較高倍率層的先發時間沿用目標握手估計的五倍，缺少樣本時使用最近一次分組 URL 測試延遲。全部先發窗口最多共用剩餘期限的一半，其餘時間留給後續候選。到時啟動下一層，已發出的撥號可繼續完成。發出應用資料的只有採用的連線。
- 記錄個別撥號的成功、真實傳輸失敗、競速取消、外層截止、嘗試截止及其他請求取消。只有真實傳輸失敗寫入節點統計。所有候選在期限內明確失敗時保留各自的錯誤。
- `StatusTest` 探測 host 根路徑。沒有 transport error 且狀態為 403 或 405 時，與原先成功狀態同樣計入可達。503 等其他 `ok=false` 狀態仍是異常。品質與恢復檢查共用該判定，原始 HTTP 請求由端點處理。

先發適用於 TCP 撥號。UDP 拿到撥號結果時依當下的嘗試 context 區分期限、取消與傳輸故障，只有傳輸故障計入節點失敗統計。已建立的 TLS 連線上的應用狀態，只能由應用端或與傳輸相關的獨立訊號判斷。[撥號](../../adapter/outboundgroup/smart_dial.go) · [回歸測試](../../adapter/outboundgroup/smart_dial_test.go)

## SC-04 持續劣化監測

**目的：** 在保留正常連線的同時，為持續低吞吐的影片連線及完全無回包的 UDP/443 出口提供可解釋、有範圍的處置。UDP 無回包採否決後續出口，影片持續低吞吐沿用既有連線劣化處置。

**納入範圍**

監測適用於目的埠 443、有 `WildcardTarget` 的 TCP 與 UDP 連線，都從既有 tracker 取流量差分，排除 `INNER` 連線。UDP 完全無回包的否決涵蓋一般網站；UDP 持續低吞吐的主動關閉仍只適用於 `googlevideo.com` 及其子網域。

**目前參數與判定：**

| 參數 | 值／用途 |
| --- | --- |
| 採樣間隔 | tunnel Running 後，每 1 秒取樣 |
| 有意義的上傳 | 觀測區段內至少 128 bytes |
| 零回應訊號 | TCP 經過至少 5 秒、區段下載仍為 0 時只輸出警告。UDP 自累計上傳達 128 bytes 的首次採樣起至少 5 秒、通道累計下載仍為 0 時否決該出口 |
| 持續低吞吐訊號 | 下載至少 16 KiB、區段至少 2.5 秒，平均下載速率低於 16 KiB/s |
| 新區段分隔 | 有新上傳、之前已有下載且距上次觀測下載至少 500 ms 時，可開始新區段；這是流量啟發式，不是 HTTP request 邊界 |
| 確認後冷卻 | 影片持續低吞吐及 UDP 完全無回包均為 5 分鐘 |

一般 TCP 在一個觀測區段內最多發出一次訊號。區段達到檢查門檻且速率健康時，結束該區段。

一般 TCP 的低吞吐是診斷訊號。有效但低流量的連線保留，快取和冷卻狀態不因此改變。正常低流量與無流量關閉可影響品質排序，單靠這些資料不建立目標故障紀錄。TCP 零回應只輸出警告。

YouTube CDN 的 TCP 和 UDP 連線只要連續採樣至少 2.5 秒、每一段都有回流但速率低於 16 KiB/s，且累計上傳至少 128 bytes、下載至少 16 KiB，即可在同一條連線確認劣化。暫停下載、完全無回包或恢復正常速率都會結束該觀測區段。手動固定節點時略過影片劣化處置。

UDP 完全無回包獨立於影片低吞吐區段取樣，因此不再因為下載為 0、未達 16 KiB 就漏掉否決。每條 association 最多產生一次這項訊號；已收到回包後的正常閒置、尚無流量或上傳不足 128 bytes 均不觸發。沿用一秒採樣，實際觸發時間包含採樣延遲。

**確認後的行為**

影片持續低吞吐沿用以下行為：

1. 冷卻以節點、wildcard 和傳輸協定為單位，期限為 5 分鐘。
2. 影片 CDN 的 winner 快取和選舉按協定隔離，停用 ASN 共用。確認劣化時清除該協定的 winner，並阻止冷卻節點寫回。
3. 只關閉被確認劣化的 tracker。tracker 以原子旗標保留關閉原因，Smart 關閉統計可辨識這是核心主動關閉。
4. `filterProxies` 排除冷卻中的候選，影片 UDP 只使用支援 UDP 的節點。候選用盡時由選路入口返回錯誤。
5. Smart 群組的 `videoDegradations` 會列出節點、協定、最近確認時間和冷卻到期時間。localClash 的增強篩選可在每輪開始時讀取這些資料，略過仍在冷卻或自上一輪取樣後確認劣化的出口。

UDP 完全無回包使用獨立的否決紀錄：

1. 冷卻範圍為此 Smart 群組內的 node＋wildcard＋UDP。所有自動候選分支均排除該出口，沒有可用候選時明確回報錯誤，不能因放寬健康條件而選回原節點。
2. 不刪除一般目標的共用 winner／ASN 快取，不寫入共用 TCP／UDP host-failure，不中斷原 association。TCP 沿用原有資格判定；其他 wildcard 不受影響。之後正常選出的 UDP winner 仍沿用既有共用快取語意。
3. 不延後 association 成功、選舉完成或封包交付。否決生效前已建立的 association 可以繼續收到遲來回包；否決影響下一次自然選點。冷卻期間的遲來回包不撤銷既有否決。
4. 否決仍有效且 UDP 關閉時累計下載仍為 0，略過原本以 `nil` error 記錄成功的品質統計，避免將零回包重新計作成功或經共用品質路徑影響 TCP。
5. 警告 `Denied unanswered UDP exit` 與 Smart 群組 `udpNoResponses` 提供 node、wildcard、觀測時間及冷卻到期時間。資料不混入 `videoDegradations`，因此不會把 UDP 專屬否決擴散到 localClash 的一般節點資格池。過期紀錄會清理。
6. 手動固定節點保留使用者選擇，不啟用這項自動否決。

**必須保留的效能／生命週期邊界：** 不攔截每次 Read／Write 重新計量；從既有 tracker 取樣。`ReaderReplaceable`／`WriterReplaceable`／`Upstream` 允許 relay 解包，避免破壞底層傳輸快路徑。monitor 等待 tunnel Running，隨 Smart context 取消而結束；Close 將連線移出監測集合。

**實作限制及同步注意：**

- tracker 以 SmartTarget 索引加上 metadata 指標相等來定位；上游若複製 metadata、改變 wrapper／tracker 建立順序或計量位置，需要重新適配。
- 冷卻及證據位於 Smart 實例記憶體中，沒有跨實例／重啟持久化。
- 低吞吐是流量與時間的啟發式證據，不能證明節點是慢速的唯一根因；例如應用本身也可能刻意限速。
- 手動選定節點的路徑會提前返回，跳過 `filterProxies` 的冷卻排除。一般 TCP 的監測沿用原有適用範圍。
- cache 已清除後才再定位 tracker；找不到 tracker 或 Close 失敗時會記錄警告，冷卻不會自動回滾。
- monitor 直接關閉的是確認連線；SC-01 所述原有 force-close 路徑仍存在，不能把整個 Smart 的關閉範圍描述成只限此連線。
- UDP 無回包否決只涵蓋 UDP/443、有效 wildcard、非 `INNER`，不是對所有 UDP 應用的回應語意檢測。無回包仍可能出自目標或網路，這項訊號表示當時出口對該 wildcard 不適合繼續優選，不宣稱整個節點不可用。

**驗證**

[monitor tests](../../adapter/outboundgroup/smart_active_monitor_test.go) 涵蓋 relay 解包、一般 TCP 低吞吐時保留 tracker 及勝者、影片間歇下載、影片 TCP 和 UDP tracker 關閉、冷卻節點無法寫回快取、下一條 UDP 連線重選和 TCP winner 隔離。實際 YouTube 播放緩衝和長時間使用待路由器驗證。

[UDP 無回包回歸](../../adapter/outboundgroup/smart_udp_no_response_test.go) 以 loopback UDP 傳送真實封包，確認一般 YouTube 網域及影片 CDN 的無回包否決、原通道仍可收到遲來回包、下一條自然請求換出口、候選耗盡不放行否決出口、TCP winner／其他目標／手動選擇不被否決，並涵蓋撥號中途否決與冷卻到期。

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
- 備援有效回覆可替換該 DoH 上游的 HTTP client，讓後續查詢重用新連線。替換須確認原 client 和選路仍是當前版本。舊 transport 的活動請求可繼續完成。一般 TCP／UDP 的 Smart 選舉仍依 SC-02 各自協調。
- 這次無回應只影響該筆查詢的備援選擇。失敗或取消保留原查詢期限和錯誤，節點品質監測仍依 SC-03、SC-04 的條件處理。

[DoH 交換](../../dns/doh_smart.go)以同一 retry ID 記錄開始備援、Smart 完成排序、備援撥號完成與收到有效 DoH。現有 [Smart 連線診斷](#sc-05-分階段診斷)仍以 TCP tracker 為範圍。備援撥號及分組銜接見 [Smart 備援](../../adapter/outboundgroup/smart_doh.go) 和 [DNS 路由](../../tunnel/dns_smart.go)。

2026-11-11 在 macOS arm64 上以相同 50 候選及 HTTP/2 測試端點重跑三輪。已快取出口的 TLS 靜默、重用連線的 DoH 靜默及冷連線 TLS 靜默，各 3/3 筆均在約 2.54 秒收到同一查詢的備援答案。另驗證固定成員、兩路靜默、原請求較慢但可成功，以及備援成功時另一筆並行請求仍可在原連線完成。端點與延遲為 loopback 模擬，正式出口上的表現待實機驗證。

## SC-07 LightGBM 啟動更新與排程重試

啟動時若現有模型已超過更新週期，Smart 會立即嘗試下載。下載失敗時保留已載入的模型與磁碟上的模型檔案，記錄原始錯誤，並在下一個排程週期向同一來源重試。下載成功且通過驗證後才寫入並重新載入模型。[LightGBM 更新程式](../../component/updater/update_lgbm.go)

回歸測試讓本機 HTTP 端點在啟動下載時傳回不完整的模型，驗證下一個排程週期仍會送出請求，且舊模型檔案保留到新模型驗證成功。[更新回歸測試](../../component/updater/update_lgbm_test.go)

## 驗證入口

### 2026-11-26 UDP 無回包否決

以 `739ecb57e023d994d57c2b7b6840687800c8e81a` 為基底，在目前 Alpha 工作目錄、Go 1.26.0／darwin arm64 驗證。新增回歸在未修改產品程式時，`www.youtube.com` 與 `googlevideo.com` 均以「unanswered UDP exit was not denied」失敗；調整後通過。

本輪保留 SC-01 的既有連線、SC-02 的 association 成功及選舉完成時機、SC-03 的撥號錯誤分類；SC-04 改用 UDP 專屬否決紀錄，SC-05 增加對應警告與狀態。CI-01 分發工作流、標籤與二進制資產均未變更，歷史上游基準也未更新。

下列命令均通過：

```bash
go test ./adapter/outboundgroup ./component/smart -count=1
CGO_ENABLED=1 go test -race ./adapter/outboundgroup ./component/smart -count=1
CGO_ENABLED=0 SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1 go test ./... -count=1
CGO_ENABLED=0 SKIP_CONCURRENT_TEST=1 SKIP_INTEROP_TEST=1 go test ./... -tags with_gvisor -count=1
git diff --check
```

完整套件沿用 build CI 的 inbound 並發／interop 跳過設定；Smart 本身的並發與 race 測試沒有跳過。UDP 封包透過 loopback 真實傳送，5 秒觀察與 5 分鐘冷卻由測試推進採樣時間，不代表已完成牆鐘時間或正式路由器實播驗收。尚未發佈、替換路由器核心或執行現場播放回驗。

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
