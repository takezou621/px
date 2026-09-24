# px

Proxmox VE ベースの軽量エージェントオーケストレータ(google/ax を参考に設計)。

## ビルド・テスト

```sh
go build ./...    # ビルド
go test ./...     # テスト(単体のみ、実 PVE 不要)
go vet ./...      # 静的解析
```

バイナリ: `go build ./cmd/px ./cmd/px-server`

## 設計の対称軸(変更時はこれに照らすこと)

- **単一バイナリ**: px-server は SQLite 埋め込み。外部 DB/メッセージブローカを追加しない。
- **Proxmox が基盤**: サンドボックスは LXC link-clone。Docker/K8s を持ち込まない。
- **ax 互換メンタルモデル**: Task/Workspace/Gateway/Model の4プリミティブ、`px.io/v1alpha1`、kubectl 風 CLI。
- **依存最小化**: REST(net/http 標準)、SQLite は modernc.org/sqlite(CGOフリー)、SSH は x/crypto。

## 実装メモ

- PVE REST API にはコンテナ内 exec がないため、ランナー起動はノード SSH + `pct exec`(internal/sshexec)。
- ランナーのブートスクリプトは全入力を base64 埋め込みにして quoting 問題を回避(internal/controller/provisioner.go)。
- コントローラは 2 秒間隔のポーリング reconcile。イベント駆動はしない(ターゲット規模が小さいため)。

## ロードマップ

docs/roadmap.md を更新してから実装を進めること。M1(単一タスクの E2E)が現在の目標。
