# Alpha fork 意圖索引與上游同步基準

本目錄記錄 `qoli/mihomo-Alpha` 相對直接上游的產品意圖、目前實作及驗證邊界。同步上游時，應保留這些行為，而不是機械保留同一份 patch 或相同函式名稱。無文字衝突不代表沒有行為漂移。

## 已核對的基準

| 項目 | 2026-09-03 核對結果 |
| --- | --- |
| 直接上游 | `https://github.com/vernesong/mihomo.git`，`Alpha` |
| 上游提交／共同祖先 | `86ece76999099381aaee80cb1410e0a8ecd2eeac` |
| fork 程式及 CI 提交 | `02c0eaf88988d524cc2b03bb212250db09866cf6` |
| 分歧 | 上游獨有 0 個提交，fork 獨有 4 個提交 |
| 差異範圍 | 9 個檔案，新增 1243 行、刪除 130 行；不含本次意圖文檔 |
| 本機 remote 注意事項 | 核對時 `origin` 指向 qoli；名稱為 `upstream` 的 remote 指向 MetaCubeX，不能當作 vernesong 的替代基準 |

這是可重現的歷史快照，不代表上游日後仍停留在此提交。MetaCubeX 是更上層的來源；本 fork 的 Smart 行為比較基準是 vernesong 的 `Alpha`。

## 意圖登記

| ID | 意圖 | 狀態 | 維護文件 |
| --- | --- | --- | --- |
| SC-01 | 成功建立新連線不淘汰既有連線 | 已實作；尚缺撥號到 tracker 的端到端回歸測試 | [Smart 連線行為](smart-connections.md#sc-01-成功撥號不淘汰既有連線) |
| SC-02 | 相同 target 的並發選路共用一次選舉 | 已實作；協調器單元測試已通過 | [Smart 連線行為](smart-connections.md#sc-02-相同-target-共用選舉結果) |
| SC-03 | 區分節點故障、競速取消與 HTTP 端點政策 | 已實作；分類 helper 測試已通過 | [Smart 連線行為](smart-connections.md#sc-03-避免把取消與端點政策當成節點故障) |
| SC-04 | 以保守的多連線證據處理持續劣化 | 已實作；訊號／證據／冷卻單元測試已通過，仍有整合驗證缺口 | [Smart 連線行為](smart-connections.md#sc-04-持續劣化監測) |
| SC-05 | 用同一 trace 區分建連、首次寫讀與關閉 | 已實作；事件時間 helper 測試已通過 | [Smart 連線行為](smart-connections.md#sc-05-分階段診斷) |
| CI-01 | fork 獨立產出可追溯的二進制分發 | 已實作；首次 rolling release 實跑通過，版本標籤路徑未實際發佈驗收 | [二進制分發](binary-distribution.md) |

「已實作」表示核對過下列提交中的程式；不是對所有平台、所有真實流量情境的保證。各文件的驗證缺口屬於現況，不是本次新增的實作計畫。

## 提交與檔案對應

| 提交 | 主要意圖 |
| --- | --- |
| `efe02b91` — stop rotating active connections on dial success | SC-01 |
| `5f29934a` — stabilize concurrent target selection | SC-02、SC-03 |
| `48e6b0c3` — Monitor sustained Smart connection degradation | SC-04、SC-05 |
| `02c0eaf8` — Enable Alpha binary distribution for this fork | CI-01 |

完整的 9 檔案覆蓋如下。純排版／檔尾換行沒有獨立產品意圖。

| 路徑 | 對應意圖 |
| --- | --- |
| `adapter/outboundgroup/smart.go` | SC-01 至 SC-05 的整合入口 |
| `adapter/outboundgroup/smart_selection.go` | SC-02、SC-03 |
| `adapter/outboundgroup/smart_selection_test.go` | SC-02、SC-03 的單元證據 |
| `adapter/outboundgroup/smart_active_monitor.go` | SC-04 |
| `adapter/outboundgroup/smart_active_monitor_test.go` | SC-04 的單元證據 |
| `adapter/outboundgroup/smart_observability.go` | SC-05 |
| `adapter/outboundgroup/smart_observability_test.go` | SC-05 的單元證據 |
| `.github/workflows/build.yml` | CI-01 |
| `README.md` | CI-01 的下載入口與操作說明 |

## 每次同步上游的流程

先在乾淨工作目錄記錄同步前的 fork SHA，擷取直接上游；下面只有讀取和 diff，沒有合併或推送：

```bash
git fetch https://github.com/vernesong/mihomo.git Alpha
fork_before=$(git rev-parse HEAD)
upstream_candidate=$(git rev-parse FETCH_HEAD)
common_base=$(git merge-base "$fork_before" "$upstream_candidate")
git log --oneline "$common_base..$upstream_candidate"
git diff --stat "$common_base" "$fork_before"
git diff "$common_base" "$upstream_candidate" -- adapter/outboundgroup component/smart common/net common/callback tunnel/statistic .github/workflows
```

1. 在同步分支處理變更，按 SC／CI ID 評估行為，而不只看 Git 是否能自動合併。`component/smart`、relay wrapper、tracker 或 workflow 的變更即使不碰 fork 新檔，也可能改變其假設。
2. 為每項意圖記錄處置：**保留**、**改用上游等價實作**、**需要適配**、**明確退役**。上游若已解決同一問題，可以刪除重複 patch，但必須保留行為證據與退役理由。
3. 遇到意圖與程式不一致，記作待解決漂移。先查明原因；不能單純改文檔來把回歸合理化，也不能為保留舊 patch 而丟棄上游新行為。
4. 執行各文件的適用驗證，重點檢查沒有文字衝突的語意變化。測試通過仍不足以宣稱真實網站延遲或連線保留已獲驗收。
5. 在同步 PR／變更說明附上下表；完成審查後，於同一變更更新本索引的基準、狀態及相關文件的實作引用。合併後共同祖先可能前移，不能只靠新的三點 diff 重建已存在的意圖。

同步記錄格式：

| ID | 上游相關提交／變化 | 處置及理由 | 驗證證據 | 尚未確認 |
| --- | --- | --- | --- | --- |
| SC-01 … CI-01 | 填寫實際影響；無影響亦註明 | 保留／等價取代／適配／退役 | 測試命令、CI URL、實測紀錄 | 明確列出 |

目前尚未建立定期同步上游或自動開同步 PR 的 Action。本文件先提供其日後必須遵守的審查依據。
