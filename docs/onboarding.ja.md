# px オンボーディング

[English](onboarding.md) | 日本語

> この文書は [onboarding.md](onboarding.md) の日本語版です。内容に齟齬がある場合は
> 英語版が正です。

ようこそ。この文書は clone から最初の貢献までを 30 分ほどで導きます。地図であって
地形そのものではありません: 各セクションは、詳細を保持するファイルを名指しします。
読む順序: このページ → 各セクションが指すファイル → 設計判断の「なぜ」が
必要になったら [roadmap.md](roadmap.md)。

## 1. px とは (3 分)

px は自律型 AI エージェントのワークロード(Claude Code、Codex CLI、OpenCode、…)を、
使い捨ての Proxmox LXC サンドボックスで実行します。プレーンな YAML で宣言し、
kubectl 風の CLI で操作します。対象はホームラボや小規模チームの Proxmox ボックス —
[google/ax](https://github.com/google/ax) とは spectrum の逆側です。px は ax の
コンセプトモデルを借用しつつ、その運用上の重さを捨てました。

4 つの設計対称軸(`CLAUDE.md` より) — あらゆる実装判断はこれに照らします:

1. **単一バイナリ** — `px-server` は SQLite(WAL)を埋め込みます。外部 DB も、
   メッセージバスも、gRPC も持ち込みません。
2. **Proxmox が基盤** — サンドボックスはテンプレートからの LXC link-clone です。
   Docker/K8s は持ち込みません。
3. **ax 互換のメンタルモデル** — 宣言的マニフェスト(`px.io/v1alpha1`)、
   4 プリミティブ + Schedule、kubectl 風の動詞。
4. **依存最小化** — REST は net/http、SQLite は modernc.org/sqlite(CGO フリー)、
   SSH は x/crypto。新たな依存を必要とする機能は、まず設計の議論です。

non-goals(意図的): Kubernetes バックエンド、gRPC API、マルチクラウド、独自
エージェントフレームワーク。px は既存のエージェント CLI を実行するもので、
エージェントを実装しません。

## 2. 全体像 (5 分)

```
px CLI ──HTTP──▶ px-server (single binary) ──PVE API──▶ Proxmox VE
                   ├─ REST API              │            └─ LXC sandboxes
                   ├─ controller (2s tick)  └─ node SSH ─▶ pct exec / cgroup freeze
                   └─ store (SQLite, WAL)
```

**5 つの kind**(`internal/apis/v1alpha1/types.go`): Task(サンドボックスで
エージェントコードを実行)、Workspace(git リポジトリを取り込む)、Gateway
(LXC ファイアウォールによる egress allowlist)、Model(LLM クレデンシャルを
環境変数として注入)、Schedule(cron で Task を量産 — Kubernetes CronJob の
セマンティクス: 停止中の発火は 1 つに圧縮)。

**Task ライフサイクル** — すべてのフェーズ遷移は、宣言的レコードに対する
reconcile ループが駆動します:

```
Pending ──▶ Provisioning ──▶ Running ──▶ Succeeded | Failed
                │  (clone → verify unprivileged →      │
                │   gateway fence → session restore    │
                │   → boot → poll exit code)           ▼
                ▼                              TTL cleanup / delete
        ProvisionFailed                        (capture session first)
Running ⇄ Suspending ⇄ Suspended ⇄ Resuming   (cgroup-v2 freeze,
                                                declarative like delete)
```

コードを読む前に内面化すべき 3 つの性質:

- **宣言し、reconcile する。** `spec` は望ましい状態です。コントローラは
  2 秒ごとにポーリングし、現実をそこへ向かわせます。suspend とポート転送は
  毎 tick 再適用されるため、場外の解凍や殺された socat は検出されるだけでなく
  修復されます。
- **apply はコントロールプレーンのみ。** apply が PVE に触れることはありません
  (kubectl の先例: apply はイメージの存在を確認しない)。未知のイメージ、欠けた
  テンプレート、削除済み参照は provision 時に `ProvisionFailed` として解決し、
  理由がレコードに名前で記録されます。
- **status は毎 tick 書き戻される。** 重いものや一生に一度のものは、Task status
  ではなく専用の SQLite テーブルに置きます(`sessions`、`events`)。status が
  持つのはフラグとポインタだけです。

**state の所在**: SQLite テーブル 7 つ(`tasks`、`workspaces`、`models`、
`gateways`、`schedules`、`sessions`、`events`)が 1 ファイルに。コンテナ状態の
source of truth は PVE のまま(px は再観測し、キャッシュしない)。ランナーの会話は
capture までコンテナ内のファイルとして生きます。境界の全体図は
[threat-model.md](threat-model.md)。

## 3. コードツアー (10 分)

非テストの Go 約 17k 行、17 ファイル。大きい順:

| パッケージ | ファイル | 役割 | 入口 |
|---|---|---|---|
| `internal/controller` | provisioner.go (1412), controller.go (1244), schedule.go (182) | reconcile ループと、PVE に対するその全仕事 | controller.go の `Controller.Run` |
| `cmd/px` | main.go (1151) | CLI 全体: apply/get/describe/logs/exec/run/watch | `main`、それから 1 つの動詞 |
| `internal/store` | store.go (858) | SQLite: 7 テーブル、単一ステートメント json_set の `Mark*` 規約 | 先頭のスキーマブロック |
| `internal/server` | server.go (841) + metrics/watch/auth | REST ルーティング、ハンドラ、NDJSON watch、bearer auth | `New` の mux |
| `internal/apis/v1alpha1` | types.go (632), manifest.go (258), cron.go (188), templates.go | 語彙: kind、フェーズ、validation caps、cron パーサ | types.go |
| `internal/sshexec` | exec.go (547) | ノード SSH エグゼキュータ: pinning、再ダイヤル、one-shot と retry の経路 | `Executor` 型 |
| `internal/proxmox` | client.go (431) | PVE REST クライアント(token auth、clone/start/stop/delete、exitstatus) | `Client` |
| `cmd/px-server` | main.go (204) | フラグ、配線、起動時の fail-fast dial | `main` |

最短の入り口は、1 つのタスクの命を追うことです:

1. `types.go` — 語彙(kind、フェーズ、validation caps)。
2. `manifest.go` — YAML が入り、検証済みの型が出る。マルチドキュメント apply。
3. `server.go` の `handleApply` → `store` — レコードの誕生(apply バッチは
   1 トランザクション、secret の redaction、placeholder の拒否)。
4. `controller.go` の `Run` → `reconcileAll` — 2 秒 tick。quota ゲート →
   provision → poll → 終端の確定 → TTL → session capture → schedule 発火。
5. `provisioner.go` の `provision` — 長い 1 本: テンプレート解決、ノード選択
   (クラスタモード)、link-clone、unprivileged 検証、gateway フェンス、
   ポート転送、session 復元、base64 ブートスクリプト、`booted` マーカーの poll。
   `ProvisionFailed` で見る失敗理由の多くは、このファイルが持ちます。
6. `sshexec` と `proxmox` — その下の 2 つのトランスポート(SSH は
   `pct exec`/cgroup freeze、REST はライフサイクル)。分割に注意: 設定的な
   書き込みは PVE API 経由(quoting の落とし穴がない)、exec は SSH 経由 —
   この境界は意図的です。

あらゆる場所で出会う 2 つの規約:

- **ブートスクリプト埋め込み**: ランナーが必要とする入力はすべて、1 つの
  base64 blob として 2 つのシェル(SSH → `pct exec`)を横断します — quoting の
  落とし穴も、`MAX_ARG_STRLEN` の驚きもありません。session 復元はその例外にして
  証明: メガバイトは argv に入らないため、チャンク化した base64 で書き込みます。
- **破壊ガード**: px が破壊するのは、ホスト名が `px-<task>` のコンテナだけです
  (`DestroyOwned`/`ErrNotOwned`) — 再利用された VMID は、px が破壊してよいもの
  では決してありません。「not found」の読み取りは構造的(`IsNotFound`)で、
  substring マッチでは決してありません。

## 4. 開発ループ (5 分)

```sh
go build ./...      # ビルド。バイナリ: go build -o px ./cmd/px && go build -o px-server ./cmd/px-server
go test ./...       # 単体テスト — PVE 不要、全部 fake
go vet ./...
```

- **単体テスト**は fake に対して動きます(プロセス内 PVE HTTP サーバ、プロセス内
  SSH サーバ、一時ファイルの SQLite ストア)。テストで固定できる機能は、まずここへ。
- **実ノード E2E** はマイルストーンの締めのステップであり、任意ではありません —
  複数のバグ(`pct exec` の PATH、pve-firewall のタイミング、yaml タグの大文字
  小文字)はそこにしか存在しませんでした。`scripts/e2e-*.sh` に 14 本のスクリプトが
  あります。セットアップ、各スクリプトが固定するもの、トラブルシューティング表は
  [e2e.md](e2e.md) へ。実行しないとしても §3(smoke)は読んでください —
  システムが実際に何をするかの、最速のツアーです。
- **プロセス**: 実装の*前に* [roadmap.md](roadmap.md) を更新する(設計記録であり、
  次の読者は「なぜ」をそこで見つける)。非自明な変更を着地させた後は、二重
  エージェントレビューを回す(同じ diff に対する独立の Codex パス + 独立の Claude
  パスを著者が評価 — 修正は merit で採用し、採用/却下のリストはマイルストーンの
  セクションに記録)。

## 5. さらに深く (5 分)

- [roadmap.md](roadmap.md) — 設計記録。各マイルストーンは、問題、選んだ設計、
  明示的な「やらない」リスト、レビューが何を捕らえたかを述べます。M13(quota)が
  最新で、実例として読めます。
- [e2e.md](e2e.md) — 実ノードでの検証、スクリプトごと。
- [threat-model.md](threat-model.md) — 受け入れたトレードオフ(LXC の共有カーネル、
  暗黙の DNS allow を通る DNS トンネルでの exfil、egress ゲートより前の
  image-init ウィンドウ)。セキュリティに隣接するものに触れる前に読むこと。
- [template/README.md](../template/README.md) — px が clone できる LXC テンプレートが
  満たすべき契約。

良い最初の貢献: 実ノードで smoke E2E を回し、それから `provisioner.go` の
`provision` を、それが生成するブートスクリプト(`template/runner/`)と並べて
読むこと。「YAML マニフェスト」と「Claude を実行する凍結された Debian コンテナ」の
間の溝こそがシステムの心臓部で、それはその 2 箇所にすべてあります。
