# px

**Proxmox VE ベースの軽量エージェントオーケストレータ。**

[English](README.md) | 日本語

> この文書は [README.md](README.md) の日本語版です。内容に齟齬がある場合は英語版が正です。

px は自律型 AI エージェントのワークロード(Claude Code、Codex CLI、OpenCode、…)を、
使い捨ての Proxmox LXC サンドボックス上で実行します。プレーンな YAML で宣言し、
`kubectl` 風の CLI で操作します — Kubernetes も、Redis も、外部コントロールプレーンも不要です。

[google/ax](https://github.com/google/ax) に着想を得ています。ax が Kubernetes +
Agent Substrate 上の数十億エージェント規模のクラスタを狙うのに対し、px はその対極 —
ホームラボや小規模チームの Proxmox ボックス、単一の Go バイナリ、組み込みの
SQLite ストアを対象とします。

```
px CLI ──HTTP──▶ px-server (single binary, SQLite) ──PVE API──▶ Proxmox VE
                                                                    └─ LXC sandboxes
```

## 設計原則

1. **単一バイナリ。** `px-server` はデータストア(SQLite/WAL)を埋め込みます。`px` は
   静的クライアント。クラスタも、メッセージバスも、サービスメッシュもありません。
2. **Proxmox が基盤。** サンドボックスはテンプレートから link-clone した LXC コンテナ
   です: copy-on-write、数秒で起動、カーネルレベルの分離。
3. **宣言し、スクリプトを書かない。** ワークロードは YAML マニフェスト
   (`px.io/v1alpha1`)として宣言し、`px apply` / `px get` / `px describe` で
   適用・参照します。
4. **デフォルトでサンドボックス。** タスクにはリソース制限が付き、Gateway を
   添付すれば egress フェンスも付きます。エージェントコードは、証明されるまでは
   信頼されません。

## プリミティブ

ax と同じメンタルモデル、独自の API グループ(`px.io/v1alpha1`):

| Kind | 役割 |
|---|---|
| **Task** | CPU/メモリ制限付きのサンドボックス LXC コンテナでエージェントコードを実行する |
| **Workspace** | git リポジトリをタスクのコンテナに取り込む |
| **Gateway** | egress トラフィックを allowlist に制限する |
| **Model** | タスクが使える LLM クレデンシャルを設定する |

## 例

```yaml
apiVersion: px.io/v1alpha1
kind: Workspace
metadata:
  name: myapp
spec:
  git:
    repo: https://github.com/example/myapp.git
    branch: main
---
apiVersion: px.io/v1alpha1
kind: Task
metadata:
  name: fix-bug-123
spec:
  image: px-runner-debian12        # PVE ノード上の LXC テンプレート
  workspaces:
    - name: myapp
      goal: "Fix bug #123 and make the tests pass"
  runner:
    # $GOAL は workspaces の goal から設定される環境変数です。コマンドは exec 形式で
    # (各 argv 要素がそのまま渡されて)実行されるため、変数展開にはシェルを挟みます。
    command: ["bash", "-lc", "IS_SANDBOX=1 claude -p \"$GOAL\" --dangerously-skip-permissions"]
  resources:
    cores: 4
    memoryMB: 8192
  ttlSecondsAfterFinished: 3600
```

```console
$ px apply -f task.yaml
workspace.px.io/myapp created
task.px.io/fix-bug-123 created

$ px get tasks
NAME          PHASE    CONTAINER   AGE
fix-bug-123   Running  142         5s

$ px watch
fix-bug-123 Provisioning -> Running
fix-bug-123 Running -> Succeeded

$ px logs -f fix-bug-123

$ px exec fix-bug-123 -- ps aux     # 動いているサンドボックスの中を覗く

$ px suspend fix-bug-123            # コンテナを凍結する (cgroup v2)
$ px resume fix-bug-123             # 凍結を解除する
```

## エージェントの実行

`px run` は 1 コマンドの近道です: フラグから Task マニフェストを組み立て、apply し、
タスクが終端フェーズに達するまでログを追います(Ctrl-C でデタッチ — タスクは
実行し続けます):

```console
$ px run -model my-claude -workspace myapp="Fix bug #123" "make the tests pass"
```

`px-agent-debian12` テンプレート(`template/agent/build.sh` — ランナーテンプレートに
Claude Code CLI を足したもの)を前提とします。デフォルトのランナーコマンドは
`IS_SANDBOX=1 claude --dangerously-skip-permissions -p "$GOAL"` です: 分離の境界は
サンドボックスであって、CLI のパーミッションプロンプトではありません
([docs/threat-model.md](docs/threat-model.md) 参照)。`-- COMMAND...` で
別のコマンドを実行できます。

## クイックスタート(目標)

```console
$ go install ./cmd/px ./cmd/px-server
$ px-server \
    --pve-endpoint https://pve.example.com:8006 \
    --pve-token PVE@px=<token> \
    --pve-node pve1
$ px apply -f examples/hello-task.yaml
```

`--pve-node` を省略すると PVE クラスタ全体に対して動作します: タスクは、イメージを
持ち、かつ空きメモリが最大の online ノードに配置されます
(`--ssh-host-override "node1=10.0.0.1,node2=10.0.0.2"` で PVE ノード名を SSH ホストに
対応付けます — ノード名は DNS 名ではないため)。

## ドキュメント

- [docs/onboarding.md](docs/onboarding.md) — まずここから: clone から最初の貢献まで
  30 分のツアー。(日本語版: [docs/onboarding.ja.md](docs/onboarding.ja.md))
- [docs/architecture.md](docs/architecture.md) — システムがどう組み上がっているか、
  as-built で。
- [docs/roadmap.md](docs/roadmap.md) — マイルストーンごとの設計記録: 何が存在し、
  なぜそうしたか、何を明示的に「やらない」と決めたか。
- [docs/e2e.md](docs/e2e.md) — 実ノードでの検証、スクリプトごと。
- [docs/threat-model.md](docs/threat-model.md) — サンドボックスの境界と、px が
  受け入れるトレードオフ。

## ステータス

Pre-alpha。API(`px.io/v1alpha1`)は変更されます。何が存在し、次に何が来るかは
[docs/roadmap.md](docs/roadmap.md) を見てください。

現在のマイルストーンでの既知の制限:

- `px-server` を `-token-file <file>` 付きで起動しない場合、HTTP API は認証なしです
  (付けた場合は全リクエストに `Authorization: Bearer` が必要になり、クライアントは
  `-token` / `PX_TOKEN` で値を渡します)。いずれにせよサーバはデフォルトで
  `127.0.0.1` にバインドします — TLS なしでポートを公開しないでください。
- `px-server` を `-ssh-host-key <file>` 付きで起動しない限り、SSH ホストキーは
  検証なしで受け入れます(同オプションはピン留めした公開ホストキーを 1 行 1 個で
  並べたファイルを取ります — `ssh-keyscan -t ed25519 <host>` で生成できます)。
- `px delete` は非同期です: レコードとコンテナは、コンテナの破壊が済んだ後、
  次の reconcile tick(約 2 秒)までに消えます。破壊に失敗した場合は成功するまで
  リトライされるため、レコードがコンテナを取り残すことはありません。

## ライセンス

Apache-2.0(このプロジェクトの着想元である google/ax に合わせて)。
