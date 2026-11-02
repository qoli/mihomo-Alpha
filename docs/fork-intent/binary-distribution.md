# CI-01：Alpha fork 二進制分發意圖

狀態：已實作；分發入口歷史核對至 `02c0eaf8`，2026-11-02 增加 Linux ARM64／AMD64-v1 的 UPX 包裝，2026-11-03 完成 UPX Build workflow 與公開 rolling 資產驗證。獨立 Test 有 macOS runner 容量取消，版本化 Release 尚未驗收。基準見[索引](README.md)。

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
10. **UPX 是明確的平台包裝政策。** 僅預設 Go 的 Linux `arm64`、`amd64-v1` matrix entry 設 `upx: true`；其他架構、AMD64 別名及舊 Go 組合維持原包裝，不推論它們都適合 UPX。使用固定 [UPX 5.3.0](https://github.com/upx/upx/releases/tag/v5.3.0) 的 Linux AMD64 host 工具，下載 archive 的 SHA-256 固定為 `79cea8a50b6fa102d10c7b79a419cd77dead3cc2d78661de2dc5e5815dc505c1`。順序為 `go build` → `upx --no-env -9 --lzma` → `upx -t`／執行驗證 → GZIP／套件 → checksum；不改 `with_gvisor` 或 Go linker flags。啟用 job 任一步失敗即失敗，不退回未壓縮 binary。ARM64 透過 `qemu-aarch64`，AMD64 原生執行；壓縮前後核對完整 `-v`、版本／架構／`with_gvisor` 與獨立目錄中的 Smart 配置測試。最終 GZIP 再解包逐 byte 比對並做 `upx -t`。同一 job 的 DEB／RPM／Pacman 也使用已壓縮的 `mihomo`；Docker 建置保持原行為。

每個 build artifact 包含封裝後的 `checksums.txt`，PR 候選也可獨立校驗。Release jobs 合併 artifacts 後仍重新產生完整 catalog，不能把 merge 過程中留下的單一 job catalog 當成 Release 的完整清單。UPX job summary 記錄當次 source SHA、版本、固定工具／參數及壓縮前後 executable bytes。

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

### 2026-10-25：DT-01 修正的 rolling 發佈

[Build 40197198909](https://github.com/qoli/mihomo-Alpha/actions/runs/40197198909)
對應 source `ab65458476de55da11da87e86586de9fee579cc8`，90 個 build 組合及 rolling
Release／Docker jobs 成功；版本化 Release job 依原條件跳過。本次沒有驗收版本化 tag 路徑。

- `Prerelease-Alpha` 的 tag／body Source commit 與來源一致，`version.txt` 為
  `alpha-smart-ab65458`；128 個公開檔案中，126 個 checksum catalog 項目與 GitHub digest 一致。
- 實際下載 `version.txt`、`checksums.txt` 與 Linux ARM64 gzip，校驗公開 digest、大小及
  ARM64 checksum；gzip SHA-256 為
  `2e99eeeae4b139efecfcdf00e9e2fbb80850fafeee9c56aaf134573ca9587b68`。
- 解壓後 ARM64 binary SHA-256 為
  `fae223331a5dfbb755572c6055661e04a0ea5181f7bc7b5b16b3b5d006d1dd62`，
  Go build info 的 revision／Linux／ARM64 皆吻合。之後正式 router 一鍵更新完成，
  活躍 executable SHA／controller 版本與此公開檔案一致；Safari 官方及本機99／96均完成
  真實 FairPlay 授權與持續影格輸出，見 [DT-01 現場回驗](tcp-destination.md#正式發佈與現場回驗)。
  不能把其餘平台的 digest catalog 校驗當成執行通過。
- 安全證據位於 macOSAgentBot 的
  `.runtime/viutv-live/cdn403-20261025/releases/mihomo-ab654584/public-assets-verification.json`。
  rolling Release 的 `publishedAt` 保留首建日期；本次身分使用 source commit、版本、
  workflow 與各資產 `updatedAt`／digest，而非以該日期反推版本。

### 2026-11-02：UPX 本地候選驗證（5ML-164，未發佈）

核心 source 為 `e860c272f9d5fca5efebd1a5078b94c1e8802976`，版本
`alpha-smart-e860c272`，BuildTime `2026-11-02T15:40:00Z`；本次僅修改 workflow 與意圖文件。
使用 workflow 同來源的 MetaCubeX Go 1.26 toolchain（此次解析為 `go1.26.10 darwin/arm64`），
交叉編譯時保留 `CGO_ENABLED=0`、`with_gvisor`、`-trimpath`、`-w -s -buildid=` 及 static linker flags。
工具 archive `go1.26.darwin-arm64.tar.gz` 的 SHA-256 為
`18ec7eefd48747a8e4b0bcbdb5303c3ba6d5ae4469ae9de3a65e58c6557ebf89`。
同一份編譯結果保留原檔後，再用 UPX 5.3.0 `--no-env -9 --lzma` 壓縮：

| 最終候選資產 | 原 executable bytes | UPX executable bytes | GZIP bytes | 安裝後節省 |
| --- | ---: | ---: | ---: | ---: |
| `mihomo-linux-arm64-alpha-smart-e860c272.gz` | 47,382,654 | 10,990,380 | 10,991,695 | 76.81% |
| `mihomo-linux-amd64-v1-alpha-smart-e860c272.gz` | 51,859,582 | 13,542,964 | 13,544,912 | 73.89% |

最終 GZIP 與逐 job catalog 的 SHA-256 核對通過：

```text
02481849c4bf0644ccbd56535072540637b3193a79de7c49c5fbf2d5cb6bf643  mihomo-linux-arm64-alpha-smart-e860c272.gz
77253dec066d37fca44cd28085c69b9acc6eea27b8d7397d56cfc65bce89dd7f  mihomo-linux-amd64-v1-alpha-smart-e860c272.gz
```

已實際執行的範圍：

- 從變更後 workflow 擷取 shell 執行，壓縮前後 `-v` 完全一致、保留 `with_gvisor`，
  Smart group 配置在不同暫存目錄中 `-t` 成功；兩個 executable 與最終 GZIP 解出內容均通過 `upx -t`。
  ARM64 在 ARM64 Linux 容器透過 QEMU user 8.2.2 執行，也直接原生執行；
  AMD64 在 macOS ARM64 主機上的 Linux AMD64 容器模擬環境執行。這不是物理 AMD64 裝置的紀錄。
  固定工具的下載／archive checksum／解包步驟亦在 Linux 執行成功；
  使用 AMD64 host 版 UPX 壓縮同一 ARM64 原檔，與 ARM64 host 版輸出逐 byte 相同且 `upx -t` 通過。
- localClash `bcfec32e355ceb2c935c5d4a94856afc437fc0a1` 的現有 checkout 編譯為兩個 Linux consumer。
  checkout 有既有未提交修改，但 `internal/coredownload`、`internal/mihomotest` 及
  `product_mihomo_update.go` 相對該提交沒有差異；本次未修改 consumer。
  候選由本地 HTTP 服務提供 metadata／最終 GZIP，經 `core download --flavor smart --target router`
  選到原有檔名並完成下載、GZIP 解包、chmod 及 rename，安裝結果與候選逐 byte 相同。
  再由 `mihomo config-test --json` 取得兩架構 `passed: true`、`isolated: true`、`core_type: smart`；
  來源目錄放置不可用的 `cache.db` 仍通過且該檔保持原內容，未借用 live runtime。
- actionlint v1.7.12 與全部 build shell 的 `bash -n` 通過。以解析後 YAML 比對原 workflow，
  90 個 matrix entry 去掉新增的兩個 `upx` flag 後完全相同；事件、版本準備、權限、
  Release／Docker jobs 及其完整 build gate 保持原內容。
- 另以同 source／toolchain 交叉建置 Darwin ARM64 與 Windows AMD64-v1，執行拆出的原包裝步驟。
  GZIP／ZIP 解出內容與各自未經 UPX 的原檔完全相同、最終 catalog 核對通過；Darwin `-v` 實際通過。
  Windows 僅建置與打包，未執行 Windows binary。注入 packer 失敗的 shell 邊界檢查回傳原 exit 13，
  未寫成功 summary 或繼續封裝；這是 CI 錯誤傳遞檢查，不是裝置功能驗收。

參數取捨：ARM64 的 `-9 --no-lzma` 對照為 13,964,068 bytes，壓縮約 46.4 秒；
`-9 --lzma` 為 10,990,380 bytes，包含壓縮及前後 QEMU 執行檢查約 35.4 秒。
同一原生 ARM64 Linux 容器各執行五次 `-v`，中位耗時為原檔 3.33 ms、NRV 165.90 ms、
LZMA 582.33 ms。為本任務的儲存目標選 LZMA；此短樣本不代表低階路由器的啟動或記憶體成本。

驗證邊界：這是本地候選與下載／配置測試的相容性驗證，未啟動 GitHub Actions、推送或公開發佈。
未重跑全部 90 平台建置、套件安裝、完整 iStoreOS 一鍵更新／接管流程或真實路由器流量；
未更新 localClash 功能表的實測版本，也不宣稱支援特定小閃存裝置。
歷史 rolling 發佈證據維持原日期與範圍，不能用來宣稱本次 UPX 已公開驗收。

### 2026-11-03：UPX GitHub Build 與公開 rolling 資產驗證

來源提交 `73af7fcdae639b8fce3facdae3477079b48bcbc9` 已 push 到 `Alpha`。
[Build 41476809468](https://github.com/qoli/mihomo-Alpha/actions/runs/41476809468)
為 push 觸發、attempt 1；90 個 build 組合、Upload-Prerelease 及 Docker 均成功，
版本化 Upload-Release 依事件條件跳過。Build workflow 結論為 `success`。
未因排程或 runner 問題刪減平台／Go 矩陣。

CI 使用 Go 1.26.10、`with_gvisor`、固定 UPX 5.3.0 `--no-env -9 --lzma`，實測如下：

| 公開 GZIP 資產 | 原 executable bytes | UPX executable bytes | GZIP bytes | 安裝後節省 |
| --- | ---: | ---: | ---: | ---: |
| `mihomo-linux-arm64-alpha-smart-73af7fc.gz` | 47,579,262 | 11,092,472 | 11,093,842 | 76.69% |
| `mihomo-linux-amd64-v1-alpha-smart-73af7fc.gz` | 52,039,806 | 13,643,132 | 13,645,024 | 73.78% |

兩個目標 job 的壓縮前後版本核對、`upx -t`、實際 `-v` 及隔離 Smart 配置測試均成功；
ARM64 在 GitHub AMD64 runner 上透過 `qemu-aarch64` 執行，AMD64-v1 原生執行。
下載固定 run 的兩份 artifact 後，其 GZIP、DEB、RPM、Pacman 共八個檔案與各自 catalog
核對通過；未把套件 checksum 核對視為套件安裝驗收。

公開 [Prerelease-Alpha](https://github.com/qoli/mihomo-Alpha/releases/tag/Prerelease-Alpha)
tag、Release body 的 Source commit、`version.txt`（`alpha-smart-73af7fc`）與來源一致。
從該 source 的 matrix 推導資產集合，與公開的 128 個資產完全相同；
`checksums.txt` 的 126 個 payload 項目與 GitHub asset digest 全部相符。
另實際下載 `version.txt`、`checksums.txt` 及兩個 Linux GZIP，核對各自公開 digest；
兩個 GZIP 與固定 run 的 artifact 逐 byte 相同。

```text
44aae90e7749192e4e6882e726706a95adf31a4c87a22ea918b7b29db889829c  mihomo-linux-arm64-alpha-smart-73af7fc.gz
0be88b56d64376fb6876afe25ef321309545acd2285f5ce16acc042739a1ccf0  mihomo-linux-amd64-v1-alpha-smart-73af7fc.gz
```

解出後 ARM64 executable SHA-256 為
`b824cc9f9282a90e1294a62929316c35ab7909cc9a573a96d6a59475f14051f0`，
AMD64-v1 為 `255d46169c8a1e8e700a75ab89809aeb519470b331f9cc2dbac633b07fb85c92`。
從兩個 UPX binary 還原後讀取 Go build info，revision 均為上述 source，平台分別為 Linux
ARM64 與 Linux AMD64／GOAMD64=v1。build info 同時記錄 `vcs.modified=true`；
workflow 在編譯前既有的 Update CA 步驟會更新倉庫內 CA 輸入，不能將這份紀錄稱為 clean-tree
可重建證明。來源身分同時由 run／tag／版本／revision／最終資產 hash 核對。

再從真實公開 Release 經既有 localClash `core download --flavor smart --target router`
下載兩個目標，保留產品選檔、GZIP 解包、chmod 及 rename 路徑；安裝結果與公開 GZIP 解出內容
完全相同。兩者在相應 Linux 容器執行 `upx -t`、`-v`，並透過正式 `mihomo config-test --json`
取得 `passed: true`、`isolated: true`、`core_type: smart`。這次使用真實 GitHub metadata／下載，
不是前節的本地候選 HTTP 服務；consumer 沿用前節記錄的 checkout，未修改 consumer 程式。
原始證據保存在本機暫存 `/tmp/mihomo-upx-ci.tewnfx/`，包括資產核驗 JSON、catalog、job log、
artifact、來源 build info 與隔離 config-test 結果；此路徑不是永久下載入口。

獨立 [Test 41476809542](https://github.com/qoli/mihomo-Alpha/actions/runs/41476809542)
attempt 1 的 workflow 結論為 `failure`：36 個 job 成功，6 個 `macos-latest` job 被取消。
逐一讀取六個 check-run annotations，皆為 hosted runner 多次嘗試仍未取得，以及 macOS ARM64
runner 容量不足；取消的 job 沒有 runner 或測試步驟。未把這六項列為程式測試通過，
也不將 Build／公開產物通過寫成整體 Test 全綠。

這輪沒有執行套件安裝、真實路由器啟動／記憶體成本、完整 iStoreOS 更新／接管或真實流量驗收，
不提高 localClash 功能表的實測版本。版本化 tag 發佈路徑仍保留原驗證缺口。

## 同步上游的檢查點

上游最容易覆蓋的區段是 `workflow_dispatch.inputs.version`、`Set variables`、兩個 Upload job 的 if／permissions／步驟、Docker 的 dispatch 條件，以及 README 下載目的地。

同步時逐一檢查事件表，確認依然滿足來源一致、空白 dispatch 能更新 rolling、tag push 有版本 Release 出口、沒有重新引入 Meta 強推。若上游換用 artifact/action/toolchain 版本，應按新版本重新檢查參數及權限，不以舊行號或 patch 是否套用成功作判斷。

可使用：

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 -shellcheck= .github/workflows/build.yml .github/workflows/test.yml
```

此命令核對 Actions 語法，停用外部 ShellCheck；不能替代 script 情境檢查與 GitHub 實跑。版本 script 至少檢查：Alpha rolling、明確 version、tag push、tag ref 上不同的明確 version、格式無效、已存在 tag 不同 commit，以及 annotated tag。

需要實際分發驗收時，記錄 source SHA、run URL、各 job 結論、Release／tag、完整 asset 清單與至少一個目標平台的下載 SHA-256、解壓及 `-v`。正式版本標籤須用真正要發佈的版本，不為測試任意建立正式 Release。
