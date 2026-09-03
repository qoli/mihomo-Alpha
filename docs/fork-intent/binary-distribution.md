# CI-01：Alpha fork 二進制分發意圖

狀態：已實作，核對至 `02c0eaf8`。首次 rolling release 實際完成；版本化 Release 目前只有設定、語法與版本邏輯驗證，未實際發佈。基準見[索引](README.md)。

## 目的與保留範圍

以 vernesong 的 [Build workflow](https://github.com/vernesong/mihomo/blob/86ece76999099381aaee80cb1410e0a8ecd2eeac/.github/workflows/build.yml) 為基礎，讓 qoli fork 能獨立編譯及分發自己的提交，同時保留上游的平台／Go 相容矩陣、`with_gvisor`、檔案命名、壓縮與套件格式，以及 rolling prerelease 習慣。

目前的 90 個 build 組合、128 個 Release 檔案是此快照的結果，不是未來永遠不變的數字。上游擴充或移除平台時，應記錄相容性影響，而不是為保持舊數目拒絕更新。

此意圖由 [build.yml](../../.github/workflows/build.yml) 及 [README 下載說明](../../README.md#alpha-smart-binaries)實作。`test.yml` 與 `trigger-cmfa-update.yml` 不在目前 fork patch 內；本 CI 沒有增加自動同步上游的流程。

## 事件與發佈目的地

| 事件 | 版本來源 | Release 行為 |
| --- | --- | --- |
| push 到 `Alpha` | `alpha-smart-<git short SHA>` | 更新 `Prerelease-Alpha` |
| 在 `Alpha` 手動 Build，version 空白 | 同上 | 更新 `Prerelease-Alpha` |
| push `v*` tag | 該 tag；須通過版本格式檢查 | 發佈該版本的 Release |
| 手動 Build，version 非空 | 明確輸入優先於來源 ref 名稱 | 以 workflow 的 source SHA 發佈該版本 |
| PR 目標為 `Alpha` | PR ref 衍生的建置版本 | 僅 build／測試／artifacts，不發佈 Release 或 Docker |

`docs/**`、`README.md`、`.github/ISSUE_TEMPLATE/**` 的純文件 push 沿用排除條件；不是所有 Markdown 都被排除。非 Alpha ref 的空白版本手動執行不屬於 rolling 發佈介面，不應用它來更新 `Prerelease-Alpha`。short SHA 的長度依 checkout 而異，不固定為 7 或 8 字元。

## 必須保留的契約

1. **來源一致。** 二進制內版本、檔名、`version.txt`、Release tag 與該次 workflow 的 source SHA 必須對得上。明確 version 輸入不能在 tag checkout 時被另一個名稱覆蓋。
2. **標籤不指錯提交。** 已存在的版本 tag 必須指向目前 checkout 的提交，包含 annotated tag 的 peeled commit；不一致時在版本準備階段失敗。新的版本 Release 使用 `target_commitish: github.sha`。
3. **fork 獨立性。** 發佈不依賴 `Meta` 分支，不 checkout、覆寫或強推 `Alpha:Meta`。移除的是上游的分支晉升假設，不是取消一般版本 Release 功能。
4. **明確版本失敗。** 輸入須符合 `vMAJOR.MINOR.PATCH` 加可選 `.`／`-` 後綴的目前 regex；它不是完整 SemVer parser。格式無效時失敗，不繼續產出不明版本。
5. **rolling 套件版本可追溯。** 透過已授權的 `gh api` 讀取 MetaCubeX 最新正式 tag，確認三段數字後依上游規則遞增 patch 並加入 fork 版本。API／格式錯誤直接失敗。這是 DEB／RPM 等套件版號來源，不是從 MetaCubeX 取代本次 source checkout。
6. **權限跟隨責任。** workflow 預設 `contents: read`；Release job 才有 `contents: write`；Docker job 為 `contents: read`＋`packages: write`。不恢復 `write-all`。
7. **完整 build 才發佈。** Release jobs 保留 `needs: [build]`，任一 build 失敗就不開始 Release 替換。找不到宣告的上傳檔案時須失敗。
8. **可下載及校驗。** Windows 保留 ZIP，其他二進制保留 GZIP，並保留上游套件／toolchain／vendor 輸出。rolling 與版本化 Release 都提供 `checksums.txt`；目前 hash 清單排除 `checksums.*` 及 `version.txt`，其餘檔案納入 SHA-256。
9. **手動 rolling 不漏掉既有 Docker 流程。** 空白 version 的 dispatch 也走一般 Docker metadata／build 條件。Docker 與 Release 在 build 後並行，Docker 結果不代表二進制上傳結果。

rolling Release 的說明包含 source SHA；版本化 Release 使用 GitHub 自動 release notes，避免依賴上游 `Meta` 分支歷史及硬編碼比較連結。

## 已驗證與限制

2026-09-03 的 [Build run 33698437226](https://github.com/qoli/mihomo-Alpha/actions/runs/33698437226) 是 **空白 version 的手動 dispatch**，來源為 `02c0eaf88988d524cc2b03bb212250db09866cf6`。90 個 build 組合、Upload-Prerelease 及 Docker 最終均成功。

[首次 Release](https://github.com/qoli/mihomo-Alpha/releases/tag/Prerelease-Alpha)共有 128 個檔案；當次主分支與 prerelease tag 均指向上述提交。下載 macOS ARM64 GZIP 後核對 `checksums.txt` 並執行 `-v`，回報 `alpha-smart-02c0eaf`、darwin arm64、`with_gvisor`。

該次 macOS ARM64 壓縮檔 SHA-256：

```text
ed1e6720499bd5efacd6799015516c59794e1bff7a3c9c6b05be50bb1c1c1586
```

以上證據有以下邊界：

- 固定 run URL 是歷史證據；`Prerelease-Alpha` 是會被更新的下載入口，不能用日後的同名 Release 反向證明舊檔案。
- 首次實跑證明 dispatch → build → Release；push／tag 的事件條件已檢查，但這次沒有獨立的 push 自動觸發或版本化發佈驗收紀錄。
- 上游流程在上傳前刪除舊 assets 並移動 rolling tag，因此上傳階段不是原子替換。build 失敗不會觸碰 Release；publish 中斷則可能暫時缺檔。不能把完整 build gate 寫成整個發佈流程具交易性。
- tag 一致性檢查在 build 準備階段；它不是在發佈時對併發移動 tag 的鎖定保證。
- 沒有對全部平台執行二進制；交叉編譯／打包成功不能等同每個裝置上的執行驗收。

## 同步上游的檢查點

上游最容易覆蓋的區段是 `workflow_dispatch.inputs.version`、`Set variables`、兩個 Upload job 的 if／permissions／步驟、Docker 的 dispatch 條件，以及 README 下載目的地。

同步時逐一檢查事件表，確認依然滿足來源一致、空白 dispatch 能更新 rolling、tag push 有版本 Release 出口、沒有重新引入 Meta 強推。若上游換用 artifact/action/toolchain 版本，應按新版本重新檢查參數及權限，不以舊行號或 patch 是否套用成功作判斷。

可使用：

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck= .github/workflows/build.yml .github/workflows/test.yml
```

此命令核對 Actions 語法，停用外部 ShellCheck；不能替代 script 情境檢查與 GitHub 實跑。版本 script 至少檢查：Alpha rolling、明確 version、tag push、tag ref 上不同的明確 version、格式無效、已存在 tag 不同 commit，以及 annotated tag。

需要實際分發驗收時，記錄 source SHA、run URL、各 job 結論、Release／tag、完整 asset 清單與至少一個目標平台的下載 SHA-256、解壓及 `-v`。正式版本標籤須用真正要發佈的版本，不為測試任意建立正式 Release。
