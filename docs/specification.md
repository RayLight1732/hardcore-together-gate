# Hardcore Together 仕様書

Paper+Velocity製の既存実装（`hardcore/`配下）をNeoForge+Gate（Gate/Managerの2プロセス構成）へ移行するにあたっての設計仕様。

## 0. プロジェクト名

| コンポーネント | 名称 | mod id |
|---|---|---|
| hardcoreサーバーのMOD | **Hardcore Together** | `hardcoretogether` |
| lobbyサーバーのMOD | **Parkour Lobby** | `parkourlobby` |
| プロキシ（Gate拡張） | **Hardcore Together Gate** | — |
| プロセス管理・アーカイブ・記録参照サービス | **Hardcore Together Manager** | — |

「Hardcore Together」は連帯責任（1人でも死ねば全員の挑戦終了）を表す名称。特定MODへの依存が無い汎用フレームワークであることも踏まえ、Twilight Forest等の固有名詞は含めていない（ボスは5.4節の設定ファイルで任意に指定可能）。「Parkour Lobby」は、チェックポイント機能がパルクールコースの中間セーブという位置づけであることを表す。以降、本文では簡潔さのため「hardcore MOD」「lobby MOD」と呼ぶ箇所があるが、いずれも上記の正式名称を指す。

当初「Hardcore Together Gate」はプロキシ機能とサーバー管理機能を1プロセスで兼ねる想定だったが、責務分離の観点から**プロキシ機能（Gate）とサーバー管理ロジック（Manager）を別プロセスに分割**した（1節・9節決定ログ参照）。両者は同一リポジトリ内の別バイナリとして提供される。

**両MODとも、サーバーサイドのみで完結する。クライアント側へのMOD導入は不要**で、バニラクライアントのまま接続できる。カスタムの`Item`/`Block`登録（クライアントにも同期されるレジストリコンテンツ）は行わない。Parkour Lobbyのチェックポイント用アイテム・看板も、バニラのアイテム/看板ブロックへ`minecraft:custom_data`（サーバー側のみが解釈する任意NBT）でフラグを付与する方式にする（4.2節）。これにより、hardcoreサーバー・lobbyサーバーとも、Gate側のPCF（Proxy-Compatible-Forge、1節）さえ導入すればプレイヤーはMOD無しで参加できる。

## 1. 全体アーキテクチャ

サーバーは2種類、どちらもNeoForge製。プロキシ側は**Gate（接続ルーティング・コマンド仲介）**と**Manager（hardcoreサーバーのプロセスライフサイクル管理・ワールドのバックアップ/アーカイブ・挑戦記録の参照）**の2プロセスで構成する。

| コンポーネント | 役割 |
|---|---|
| Hardcore Together Gate | 接続ルーティング、コマンドの受付・権限チェック、hardcore再起動前後のプレイヤー退避・自動転送 |
| Hardcore Together Manager | hardcoreサーバーのプロセスライフサイクル管理（`os/exec`でhardcoreを子プロセスとして起動/停止/再起動）、ワールドのバックアップ/アーカイブ実行、`running`状態管理、挑戦記録（`records/`）の直接読み取り |
| Parkour Lobby（lobbyサーバー、NeoForge） | チェックポイント機能を持つ、パルクールコース中心の待機所 |
| Hardcore Together（hardcoreサーバー、NeoForge） | RTA本体。死亡/討伐判定、タイマー、記録管理 |

旧Paper版の`ServerBooster`（プロキシ側でのプロセス管理・バックアップ）と`WorldArchiver`（ゲームサーバー側でのワールドコピー）の役割は、**Managerに統合**する。理由：
- NeoForgeには稼働中サーバーの中で任意名のワールドを動的生成・削除するBukkit相当のAPIが無いため、「新しい挑戦を始める」はサーバープロセスごとの再起動でしか実現できない
- プロセス管理とファイル操作を1箇所（Manager）に集約した方が、競合状態（同時に`/start`と`/archive`が走る等）を避けやすい

**GateとManagerを別プロセスに分離した理由**：Gateは本来「Minecraftプロトコルのルーティングを行うプロキシ」であり、OSプロセスの起動/停止やファイルシステム操作（ワールドのバックアップ/復元）はプロキシ本来の責務ではない。両者を1プロセスに同居させると単一責任の観点で境界が曖昧になるだけでなく、Dockerでのデプロイ時にGateコンテナへhardcoreの作業ディレクトリ（`world/`・`records/`）やアーカイブ保存先をマウントする必要が生じ、「Gateはネットワークのみで完結する」という単純さが失われる。そのためGateは接続ルーティングとコマンド仲介に徹し、プロセス管理・ファイル操作・状態管理はすべてManagerへ切り出した。GateとManagerは7節で後述する専用プロトコルで通信する。

**前提**：Manager・hardcoreサーバーは`os/exec`の親子プロセス関係にあるため**同一ホスト（同一コンテナ）上で動作する必要がある**。lobbyサーバーはGateから見て単なる接続先の1つであり、ホストの制約は無い。Gate自体もManagerとはネットワーク経由で通信するため理論上別ホストでも動作しうるが、運用をシンプルに保つため、デフォルトではGate・lobby・Manager（+hardcore）を同一Docker network内の別コンテナとして同一ホスト上に配置する構成を想定する。アーカイブ保存先（3.2節）はこの前提のもと、Managerから直接ファイル操作可能なローカルディスク（Dockerボリューム）上に置く。

lobby/hardcore両方がmoddedになったため、Vanilla/Paperとの混在互換性の懸念は無くなった。両サーバーにProxy-Compatible-Forge（PCF）を導入し、Velocity方式のforwardingで統一する。

```mermaid
flowchart TB
    subgraph Client["プレイヤー"]
        P[Minecraft Client]
    end

    subgraph Gate["Gate (Go, プロキシ専任プロセス)"]
        G1[ログインゲート制御]
        G2["/rta, /lobby, /start [clean],<br/>/load, /deactivate コマンド受付"]
        G3["/serverコマンド制限"]
        G5[プレイヤー退避・自動転送]
        G6[Managerからのstate/ready通知受信]
    end

    subgraph Manager["Manager (Go, 常駐プロセス)"]
        M1["hardcoreプロセス起動/停止/再起動<br/>(os/exec, 子プロセス)"]
        M2["ワールドbackup/wipe/restore"]
        M3[hardcore状態管理<br/>停止中/起動処理中/準備完了]
        M4[hardcoreのrunningキャッシュ]
        M5["records/*.json 直接読み取り<br/>(/savedata, /senpan用)"]
        M6[ready / archive-request / running-changed 受信リスナー]
    end

    subgraph Lobby["lobbyサーバー (NeoForge)"]
        L1[チェックポイント機能]
    end

    subgraph Hardcore["hardcoreサーバー (NeoForge、Managerの子プロセス)"]
        H1["/archive コマンド"]
        H2[死亡/討伐イベント処理]
        H3[running状態をSavedDataへ永続化]
        H4[起動完了・archive要求・running変化をManagerへ通知]
    end

    P -->|接続| Gate
    G2 -->|接続切替| Lobby
    G2 -->|接続切替| Hardcore
    Gate <-->|start/load依頼, state通知,<br/>savedata/senpanクエリ| Manager
    H4 -->|ready / archive-request / running-changed| M6
    M6 --> M3
    M6 --> M4
    M2 -->|archive-complete| H4
    M1 -->|プロセス起動/停止| Hardcore
    M5 -.->|records/*.json 直接読み取り| Hardcore
```

## 2. Gate仕様

以下はユーザーから見た挙動であり、実際の状態管理・プロセス制御・ファイル操作はすべてManagerが行う（3節）。Gateはコマンドの受付・権限チェック・Managerへの処理依頼・結果表示という「仲介役」に徹する。

### 2.1 コマンド（どのバックエンドに接続中でも使用可能）

**背景（旧設計の欠陥）**：当初案には、一度もhardcoreを起動したことが無い状態からでも`/start`が永遠に成功しないデッドロックがあった。

- Managerの`running`キャッシュは、hardcore MOD側からの`ready`/`running-changed`シグナル（6節）でのみ更新されるオンメモリの値であり、Manager起動直後やhardcoreとの接続が確立していない間は安全側に倒して`true`（進行中）扱いにする、という設計だった（旧6.1節「接続断の扱い」）
- `/start`はこの`running`キャッシュが`true`の間は常に拒否される
- `running`キャッシュを`true`から書き換えられるのは、hardcore自身が起動して`ready`を送ってきた場合のみ
- しかし、そのhardcoreプロセスを起動させる手段（`/start`）自体が、上記の理由で常に拒否される

つまり「一度も起動していない」という**安全に許可してよいはずの状態**を、「今まさに進行中で接続が切れているだけかもしれない」という**危険側の状態**と同じ`unknown`＝`true`扱いにしてしまっていたため、循環から抜け出せなかった。

**修正の方針**：一度検討した`/activate`・`/deactivate`という4コマンド構成は、`/start`と`/activate`の役割分担（「ワールドを作り直すか否か」）がコマンド名からは読み取れず分かりにくいという指摘を受け、**`/start [clean]`・`/load <name|latest> [force]`・`/deactivate`の3コマンドに整理し直した**。ポイントは、**`/start`（`clean`無し）を`running`値を一切参照しない操作に変える**こと：

- `/start`は「hardcoreプロセスを起動する」ことだけを行う。**判定に使うのはプロセスが既に起動中かどうか（Managerが`os/exec`で直接把握でき、常に正確）だけであり、`running`値は一切見ない。** 停止中なら、`world/`が無ければ新規作成してから起動し、あれば中身に触れず起動するだけ
- ワールドを**強制的に作り直したい**場合だけ、明示的に`clean`を付ける（`/start clean`）。中身を作り変える操作だからこそ、ここでは`running`が`true`かどうかを気にせず常に実行する（=「進行中の挑戦を壊してもよい」という意思表示そのものが`clean`の指定）
- `/load`は元々ワールドを別の内容（アーカイブ）で置き換える破壊的な操作なので、従来通り`running`チェック（`force`で免除可能）を残す

この結果、**`/start`の受理条件から`running`（hardcoreとの接続に依存する、`unknown`になりうる値）が完全に外れる**——プロセスの生死というManagerが常に正確に把握できる情報だけで判定できるようになるため、上記のデッドロックは構造的に発生しなくなる。以前検討していた「ワールドに触れずプロセスだけ起動する」という`/activate`の役割は、この新しい`/start`（`clean`無し）がそのまま担う。

**状態モデル**：`/start [clean]`・`/load`・`/deactivate`の挙動は、独立した2つの状態軸の組み合わせで決まる。

- **挑戦の状態**（`running`値、3.1節・5.1節）：`存在しない`（`world/`未作成。一度も`/start`していない）／`進行中`（`running=true`）／`終了`（`running=false`。死亡または挑戦終了系ボス討伐後）
- **hardcoreプロセスの状態**（Managerが`os/exec`で管理する子プロセスの生死、3.1節）：`起動中`（起動処理中・準備完了を合わせた状態）／`停止中`

この2軸は完全に独立している。**Managerはプロセスが停止しても自動的に再起動を試みない**——起動は必ず`/start`（`clean`の有無を問わず）・`/load`のいずれか、ユーザー起点の操作によってのみ行われる。これにより「進行中の挑戦なのにプロセスは停止している」状態（④、後述）を正常系として扱えるようになる。

「挑戦が存在しない」かつ「プロセスが起動中」の組み合わせは構造的に発生しない（プロセスが起動していれば`world/`は必ず存在するため）。以降、実質6通りの組み合わせを扱う。

| コマンド | 権限 | 動作 |
|---|---|---|
| `/rta` | 誰でも | hardcoreサーバーが「準備完了」状態なら接続を切り替える。「停止中」「起動処理中」ならその旨を表示するのみ |
| `/start` | OP | hardcoreプロセスを起動する。**プロセスが既に起動中なら拒否**（「既に起動しています」）。停止中なら、`world/`が無ければ新規作成してから起動し、あれば中身に一切触れず起動するだけ。`running`値は判定に使わない。詳細な状態別挙動は下表参照 |
| `/start clean` | OP | hardcoreプロセスの状態に関わらず**ワールドを強制的に再生成して起動する**：起動中なら退避→停止、`world/`を削除→新規作成→起動。進行中だった挑戦があれば破棄される。詳細な状態別挙動は下表参照 |
| `/load <name>` | OP | `archive/<name>`から挑戦を復元して起動する。**`running`が`true`の場合は拒否**（「挑戦が進行中です」）。詳細な状態別挙動は下表参照 |
| `/load <name> force` | OP | `running`チェックを**スキップ**する`/load <name>`。アーカイブが存在しない場合のエラー表示は通常の`/load <name>`と同じ（`force`はrunningチェックのみを免除するもので、存在チェックまでは免除しない） |
| `/load latest` | OP | `running`チェックは`/load <name>`と同様。`name`に`latest`を指定した場合、Managerが全アーカイブのメタデータ`createdAt`を比較し、最も新しいものを自動選択して`/load <name>`と同じ処理を行う。アーカイブが1件も無い場合はエラーメッセージを表示して何もしない |
| `/load latest force` | OP | `/load latest`と`/load <name> force`を組み合わせた動作（最新アーカイブを自動選択しつつ、runningチェックはスキップ） |
| `/deactivate` | OP | hardcoreプロセスを安全に停止する。**プロセスが既に停止中なら拒否**（「既に停止しています」）。起動中なら退避→停止。ワールドの中身・`running`値は一切変更しない。詳細な状態別挙動は下表参照 |
| `/lobby` | 誰でも | lobbyサーバーへ接続を切り替える |
| `/server` | 権限保有者のみ表示 | 旧`ServerCommandRestrictor`相当。無権限者には登録しない |

`/start`・`/start clean`・`/load`の「自動転送」は、保留リストやスナップショットを持たない。hardcoreプロセスが準備完了になった時点（`ready`受信時点）で、そのときlobbyサーバーに接続している全プレイヤーに対して自動接続を行うだけでよい。

**状態別挙動**（①〜⑥は挑戦の状態×プロセスの状態の組み合わせ）

| 状態 | `/start` | `/start clean` | `/load <name> [force]` | `/deactivate` |
|---|---|---|---|---|
| ①存在しない×停止中（初回、`/start`未実行） | 受理：`world/`が無いので新規作成 → 起動 → 準備完了で自動転送（③へ） | 受理：`world/`が無いので`/start`と同じ結果（新規作成→起動、③へ） | `force`不要で受理：退避不要 → アーカイブ復元 → 起動 → 自動転送（③または⑤へ、アーカイブの`running`次第） | **拒否**：「既に停止しています」 |
| ②存在しない×起動中 | *（不変条件により発生しない。プロセス起動中なら`world/`は必ず存在する）* | 同左 | 同左 | 同左 |
| ③進行中×起動中（通常運用） | **拒否**：「既に起動しています」 | 受理：退避 → 停止 → `world/`削除 → 新規作成 → 起動 → 自動転送（新しい挑戦、③のまま） | `force`無し→拒否「挑戦が進行中です」。`force`有り→退避→停止→復元→起動 → 自動転送（③または⑤へ） | 受理：退避 → プロセス正常停止。`running`値・ワールドは変更しない（④へ遷移し、進行中の挑戦は中断されたまま保持される） |
| ④進行中×停止中（プロセスのクラッシュ・ホスト再起動直後、または`/deactivate`後） | **受理（新設計の核心）**：`world/`には一切触れず、プロセスを起動するだけ → 準備完了で自動転送（③へ、中断していた挑戦を再開） | 受理：退避不要（誰も接続していない）→`world/`削除 → 新規作成 → 起動 → 自動転送（新しい挑戦、③へ） | `force`無し→拒否「挑戦が進行中です」（プロセス停止中でも`running=true`である限り変わらない）。`force`有り→退避不要→復元→起動 → 自動転送（③または⑤へ） | **拒否**：「既に停止しています」 |
| ⑤終了×起動中（死亡/クリア直後） | **拒否**：「既に起動しています」（`running=false`でも、プロセスは既に起動しているので`/start`の役目は済んでいる） | 受理：退避（念のため）→ 停止 → `world/`削除 → 新規作成 → 起動 → 自動転送（③へ） | `running=false`のため`force`不要で受理：退避（念のため）→停止→復元→起動 → 自動転送（③または⑤へ） | 受理：退避 → プロセス正常停止（⑥へ遷移） |
| ⑥終了×停止中 | 受理：`world/`には一切触れず、プロセスを起動するだけ → 自動転送（⑤へ、終了状態のまま再開） | 受理：`world/`削除 → 新規作成 → 起動 → 自動転送（③へ） | `running=false`のため`force`不要で受理：退避不要→復元→起動 → 自動転送（③または⑤へ） | **拒否**：「既に停止しています」 |

`/start`（`clean`無し）は④→③・⑥→⑤という、**ワールドを変更せずプロセスのみを起動する遷移**を担う（旧`/activate`相当）。`/deactivate`は③→④・⑤→⑥を担う。`/start clean`は**必ず新規ワールドを作るため、常に③（`running=true`）へ遷移する**（5.1節：新規`SavedData`は常に`running=true`で初期化されるため、⑤へ遷移することは無い）。`/load`だけが、復元するアーカイブ自身の`running`値次第で③・⑤どちらへも遷移しうる（アーカイブは`running=true`のまま作成されるのが通常だが、死亡/クリア後に手動`/archive`した場合はアーカイブ自体が`running=false`のスナップショットになりうるため、5.2節）。

起動処理中・停止処理中（プロセスが遷移の途中にある一瞬）に`/start`・`/start clean`・`/load`・`/deactivate`のいずれかが呼ばれた場合は、状態に関わらず一律「処理中です。しばらくお待ちください」と表示して拒否する（3.2節の排他制御と同様、Manager内部のロックで直列化する）。

**注記：アーカイブ実行中は上記6状態のどこからでも横断的にブロックされうる**。`/archive`実行中（3.2節）は、①〜⑥のどの状態であっても`/start`・`/start clean`・`/load`・`/deactivate`がManager内部の排他ロックで待たされる（拒否ではなくブロック、3.2節）。これは6状態の分類とは独立した軸（ワールドの状態でもプロセスの起動状態でもなく、「ファイルコピー中かどうか」という一時的な状態）であり、上記の状態表には現れないため見落とさないよう注意。

```mermaid
stateDiagram-v2
    S1: ①存在しない×停止中
    S3: ③進行中×起動中
    S4: ④進行中×停止中
    S5: ⑤終了×起動中
    S6: ⑥終了×停止中

    [*] --> S1
    S1 --> S3: /start, /start clean, /load(running=true)
    S1 --> S5: /load(running=false)
    S3 --> S4: /deactivate
    S4 --> S3: /start
    S5 --> S6: /deactivate
    S6 --> S5: /start
    S3 --> S3: /start clean
    S3 --> S5: /load force(running=false)
    S4 --> S3: /start clean, /load force(running=true)
    S4 --> S5: /load force(running=false)
    S5 --> S3: /start clean, /load(running=true)
    S5 --> S5: /load(running=false)
    S6 --> S3: /start clean, /load(running=true)
    S6 --> S5: /load(running=false)
```

**`force`の適用範囲**：`force`は`/load`にのみ存在し、「hardcoreの`running`が`true`の場合に拒否する」チェックを免除する。`/start`（`clean`の有無を問わず）・`/deactivate`には`force`の概念が無い——`/start`（`clean`無し）はそもそも`running`を見ないので免除するものが無く、`/start clean`も`running`に関する拒否分岐自体を持たない（免除すべき`running`チェックが元々存在しない）。`/deactivate`の拒否理由も「既に停止しています」（`running`とは無関係な、プロセス状態のみの判定）のみで、これも`force`で免除できる性質のものではない。ただし`/start`・`/start clean`・`/load`・`/deactivate`のいずれも、遷移中（`starting`/`stopping`）であれば状態に関わらず「処理中です」で拒否されうる（前述）——これは`force`・`clean`で免除できるものではなく、排他制御としての性質のものだから対象外である。3.2節の「アーカイブ実行中は`/start`・`/load`をブロックする」排他制御は`force`・`clean`のいずれでも免除しない（ファイルコピーの最中にセーブフォルダを消すのは別種の危険なので、これは常に守る）。`/start clean`・`/load force`の実行時、退避時点で`running=true`だった（＝進行中の挑戦を実際に破棄する）場合は、退避されるプレイヤーへの通知メッセージを通常の「ワールドリセットのためロビーに戻りました」から「管理者により強制リセットされました」に変える。

**プロセス状態と`running`の永続化**：hardcoreプロセスの生死はManagerが`os/exec`で直接管理するため常に正確である（不明状態は無い）。**ただしこれはManager自身が生きている前提であり、Manager自体がクラッシュして子プロセスが孤児化した場合の対策は`architecture-manager.md` 3節のPIDファイルによる起動時生存確認で別途担保している。** `/start`（`clean`無し）はこの情報だけで判定できるため、`running`の不確実性に影響されない（上記「背景」のデッドロックが起きない理由）。一方`/load`は引き続き`running`値を参照する必要があるため、その正確性を保つ目的で`running`値（挑戦の状態）は、hardcore MOD側からの6節`running-changed`シグナル（およびinitial値を含む`ready`シグナル）を受信するたびに、**Manager自身のローカルディスクへ永続化する**（オンメモリのキャッシュではなく、Managerの作業ディレクトリ内に持つ小さな状態ファイル。パスはManagerの設定ファイルで指定）。これにより、Manager自身が再起動してもこの値は失われない。この値の扱いは3通りに分かれる：

- **プロセスが停止している場合**（④・⑥）：直前に永続化した`running`値をそのまま使う。Manager自身の再起動をまたいでも失われない
- **プロセスは起動中（Managerの`os/exec`ハンドルで確認済み）だが、hardcoreとのTCP接続だけが切れている場合**（まれな異常系）：安全側に倒して`true`（進行中）扱いとする。**`unknown`はこのケースにのみ使う**——プロセスが実際に生きている以上、挑戦が進行中でない保証が無いため
- **永続化された値が一度も存在しない場合**（＝一度も`/start`・`/load`が成功したことが無い）：`存在しない`（①）として扱う（`running=false`と同じ扱いで、`/load`の`running`チェックを素通りする）

GateはManagerから見た状態をローカルにキャッシュせず、`/rta`実行時などに7節の`state-query`で都度問い合わせる（Gate側にキャッシュを持たせると、Manager⇔Gate間の接続断・再接続のタイミングでズレが生じうるため）。`/start`・`/load`・`/deactivate`は事前のキャッシュ判定を行わず、常にManagerへ要求を送って判定を委ね、拒否された場合は`start-rejected`/`load-rejected`/`deactivate-rejected`として結果を受け取る。GateはManagerとの接続が切れている間は`state-query`を送らず、状態不明として扱う。

### 2.2 挑戦記録の参照コマンド（どこからでも使用可能）

| コマンド | 権限 | 動作 |
|---|---|---|
| `/savedata` | 誰でも | hardcoreサーバーの起動有無に関わらず、5.5節の`records/<challengeId>.json`を全`challengeId`分横断して一覧表示する。MODへの問い合わせは行わない |
| `/senpan list\|count` | 誰でも | 同上のデータにより、全`challengeId`のイベントログから`type: death`のイベントを集計し、戦犯（死亡したプレイヤー）の一覧・回数を表示する |

実体の読み取りはManagerが行い、Gateは7節のプロトコルでManagerへ問い合わせて表示するだけである（詳細は3.3節）。この設計にした理由は2つ：①Manager・hardcoreは同一コンテナ上にある前提（1節）なので、Manager側はファイルI/Oだけで完結できる、②hardcoreサーバーが停止中でも過去の挑戦記録を閲覧できる方が実用上望ましいため。

**`/archive`はこの方式にしない**：`/archive`は「オートセーブを一時停止する」という**稼働中のMinecraftサーバープロセスでしか実行できない操作**を必ず伴うため、実行者がhardcoreに接続している必要が実質的にある（hardcoreが起動していなければ何もできない）。これをGate/Manager経由の中継にすると、①Manager⇔hardcore MOD間に新規のコマンド中継シグナルが必要になる、②受信スレッドで直接ブロッキング処理を行うとデッドロックしうる、③hardcoreに繋いでいないかもしれない実行者へGateがメッセージを送る仕組みが必要になる、といった複雑さが生じ、得られる利便性（hardcoreに繋がずに手動アーカイブできる）に見合わない。そのため`/archive`は5.2節の通りhardcore MODが直接登録する、従来通りの設計に留める。

### 2.3 プロセス再起動時の退避

`/start clean`・`/load`・`/deactivate`が受理され、かつその時点でプロセスが起動中（＝プレイヤーが接続している可能性がある）の場合、Managerはプロセスを止める前に7節の`evacuate-request`シグナルをGateへ送る。Gateはこれを受けて、プロセスが止まる前に必ず、hardcoreに接続中の全プレイヤーをlobbyへ強制転送し、完了したら`evacuate-complete`をManagerへ返す。Managerはこの完了通知を受けてはじめてプロセス停止に進む。理由：退避なしにプロセスをkillすると、該当プレイヤーはネットワークごと切断されてしまうため。

`/start`（`clean`無し）は退避を伴わない——プロセスが既に起動中なら拒否されるため、実行が進むのは常に「プロセス停止中」（2.1節の④・⑥）の場合のみで、hardcoreに接続しているプレイヤーは存在し得ない。`/start clean`・`/load`も、対象状態がもともと「プロセス停止中」（④・⑥）の場合は同様に退避が不要——`evacuate-request`を送るかどうかはManagerが「その時点でプロセスが起動中だったか」で判断し、Gate側はいずれの場合も`evacuate-request`を受信したときだけ退避処理を行う点は変わらない。

### 2.4 異常切断時のフォールバック

`KickedFromServerEvent`をフックし、hardcoreから予期せず切断された場合は自動でlobbyへの再接続を試みる。上記2.3の退避処理が漏れた場合やクラッシュ時のセーフティネット。

## 3. Manager仕様

### 3.1 hardcore状態管理

Managerはhardcoreプロセスのライフサイクル（`os/exec`での起動/停止）を自身で管理する。プロセスの状態は内部で4値管理する。

```
停止中 → (/start・/load 受理) → 起動処理中 → (ready受信) → 準備完了
準備完了 → (/start clean・/load force 受理) → 起動処理中 → ...
準備完了 → (/deactivate 受理) → 停止処理中 → (プロセス終了確認) → 停止中
```

`/start`は停止中からのみ受理される（起動中なら拒否、2.1節）ため、この遷移図の「準備完了 → 起動処理中」を経由するのは常に`/start clean`・`/load force`のみである。`/start`（`clean`無し）・`/load`（`force`無し、`running=false`の場合）はいずれも「（必要ならワールドの新規作成/復元）→ プロセス起動」という共通の後半手順を持つ（2.1節）。

この4値のプロセス状態は、2.1節の`running`（挑戦の進行状態）とは完全に独立した別軸である。プロセス状態はManagerが`os/exec`のハンドルから常に正確に把握できる一方、`running`はhardcore MOD側からのシグナルに依存するため、Manager自身のローカルディスクへ永続化してManager再起動をまたいで保持する（2.1節「プロセス状態と`running`の永続化」）。Managerはこの2つの状態を、7節の`state-query`を受信するたびにその時点の値で応答する（`state-response`）。Gateはこれを都度問い合わせ、`/rta`は「準備完了」以外では接続を試みない（旧LoginControlが生死確認をしていなかったバグの修正）。

### 3.2 アーカイブ実行

`archive-request`受信時、Managerはhardcoreサーバーのプロセスを止めずに、現在のセーブフォルダを`archive/<name>/`へコピーし、付随するメタデータ（経過タイム`elapsedTime`・作成日時`createdAt`）をJSONとして保存する。`createdAt`は**Manager自身が処理時点の現在時刻から生成する**（MODからは送られない。6節参照）。`/load latest`で最新アーカイブを判定する際の比較キーとして使う。`/start`・`/load`によるプロセス再起動と同時に走らないよう、進行中はどちらか一方をブロックする。

**`archive/<name>/`の実体**：これは**Manager自身が管理する保存先ディレクトリ**であり、パスはManagerの設定ファイル（例：`config.yml`の`archiveDir`）で指定する。hardcoreサーバーの作業ディレクトリ（`world/`などセーブフォルダを含む場所）とは別の場所に置く——`/start`で削除されるのは`world/`（セーブフォルダ）のみで、`archive/`はManagerが管理する別ディレクトリなので影響を受けない。前提として、**Managerとhardcoreサーバープロセスは同一コンテナ（ホスト）上で動作する**（Managerが`os/exec`で直接プロセスを起動/停止する設計のため、リモートホストでの実行は想定していない。1節参照）。

5.5節の`records/<challengeId>.json`（MOD管理、hardcoreサーバーのセーブフォルダの外）とは別物である点に注意：`archive/`はワールドの実体（セーブデータそのもの、Manager管理）、`records/`はイベントの記録（メタデータのみ、MOD管理）という役割分担になっている。

**`name`の生成元と名前の重複**：`archive-request`の`name`は**任意フィールド**であり、その有無だけでManagerの挙動が一意に決まる（手動/自動を区別する専用フィールドは持たない、6節）。
- **`name`を送った場合**（手動`/archive <name>`）：MOD指定の`name`をそのまま使う。`archive/<name>/`が既に存在する場合は`archive-request`を拒否する（上書きしない）。MODへ拒否を返し、MODはOPへ「その名前は既に使われています」と表示する
- **`name`を省略した場合**（ボス討伐による自動アーカイブ）：**Manager自身が処理時点の現在時刻から`<討伐時点相当の日時>`形式の名前を生成する**（例：`2026-07-18T12-34-56`、秒単位以上の精度を持つタイムスタンプ。討伐時点の日時をMODが計算・整形して送る必要が無くなった）。同一秒内に複数のボスが討伐される稀なケースに備え、衝突時はManager側で末尾に連番を付与して回避する（`2026-07-18T12-34-56-2`等）。拒否せず継続させる

Managerが実際に採用した名前（連番付与後の最終的な名前を含む）は`archive-complete`の`name`でMODへ返す（6節）。MODは5.5節のイベントログへ`archiveName`を記録する際、`name`を省略していた場合はこの`archive-complete`の`name`を使う（MOD側では最終的な名前を事前に知り得ないため）。

サーバーを動かしたままの安全なコピーには`save-all flush`だけでは不十分（コピー中に次のオートセーブが走ると、一部ファイルだけ新しい状態になった「歪んだ」アーカイブができるリスクがある）。バニラ標準の`save-off`/`save-on`で書き込みを一時停止する手順を踏む：

1. MOD側で`save-off`（以降のオートセーブを一時停止。サーバー自体は動き続ける）
2. MOD側で`save-all flush`（この時点までの状態を即時・強制的に書き込む。`save-off`後でも`/save-all`は`noSave`フラグを無視して実行される）
3. MODがManagerへ`archive-request`を送信
4. Managerがファイルコピーを実行し、完了したら`archive-complete`をMODへ返す
5. MODが`archive-complete`受信で`save-on`（オートセーブ再開）

`/archive`コマンド自体は5.2節の通りhardcore MODが直接持つ（Gateを経由しない）。理由は2.2節参照。

### 3.3 挑戦記録の読み取り

`/savedata`・`/senpan`が参照する記録、および経過時間の継続計算は、すべて5.5節の仕組みに一本化する。アーカイブ機構（`archive-request`）とは独立しており、`/archive`を一度も実行しない挑戦であっても、死亡・クリアの結果は必ず記録される。

Managerは書き込みを行わない（書き込みはhardcore MODの責務）。Manager・hardcoreは同一コンテナ上にある前提（1節）のもと、Managerが`records/<challengeId>.json`を直接読み取り、全`challengeId`分横断して`/savedata`・`/senpan`のレスポンスを組み立て、7節のプロトコルでGateへ返す。

**設定を分ける2つのプロセスが同じ場所を見る必要がある**：`storage.recordsDir`はhardcore MOD自身の設定ファイルの値だが、Managerも同じ場所を読みに行く（本節）ため、Manager側の設定（例：`config.yml`の対応する値）と必ず一致させる必要がある。値を変更する場合は両方を同時に更新すること。

## 4. Lobbyサーバー仕様（Parkour Lobby、NeoForge）

チェックポイント機能のみ。

### 4.1 データ永続化

プレイヤーごとのチェックポイント座標は`AttachmentType`でプレイヤーに紐づけて保存する（旧Paper版の`PersistentDataContainer`を直接プレイヤーに付与していたのと同等）。

### 4.2 コマンド

| コマンド | 権限 | 動作 |
|---|---|---|
| `/checkpoint` | 誰でも | 保存済みチェックポイントへテレポート。未保存ならロビースポーンへ |
| `/checkpoint reset` | 誰でも | 自分のチェックポイントを削除 |
| `/checkpoint reset <target>` | `checkpoint.reset` | 対象プレイヤーのチェックポイントを削除 |
| `/checkpoint give checkpoint_item\|reset_item\|checkpoint_sign\|reset_sign` | 誰でも | 該当アイテムを自分に付与 |
| `/checkpoint give ... <target>` | `checkpoint.give` | 対象プレイヤーに付与 |
| `/checkpoint help` | 誰でも | ヘルプ表示（OPには管理者コマンドも追加表示） |

チェックポイント用の看板・アイテムは**バニラのアイテム/看板ブロックをそのまま使う**（例：`minecraft:oak_sign`、任意のバニラアイテム）。新規`Item`/`Block`は登録しない（0節の「サーバーサイドのみで完結する」制約のため。カスタムレジストリコンテンツはクライアントにも同期が必要になり、MOD無しの接続ができなくなる）。フラグの持たせ方：
- アイテムは`minecraft:custom_data`（バニラのData Component、任意のNBTを保持できる。クライアントは中身を解釈しないが保持はする）へ、例えば`{"parkourlobby": {"kind": "checkpoint_sign"}}`のようなタグを埋め込む
- 表示名・Loreはバニラの`minecraft:custom_name`/`minecraft:lore`コンポーネントで独自に付与する（バニラクライアントでも通常のアイテム名/説明文として表示される）
- 看板は設置後、対応する`BlockEntity`側にも同様のカスタムNBTを保持させ、`PlayerInteractEvent`/設置イベントでこのNBTの有無を見て判定する

サーバー切替系のコマンド（旧`/rta`テレポート相当）はlobby MODには持たせない。すべてGate側の`/rta`・`/lobby`に統一する。

## 5. Hardcoreサーバー仕様（Hardcore Together、NeoForge）

### 5.1 状態永続化

`running`（進行中フラグ）・`challengeId`（挑戦ID、5.5節）を`SavedData`に保存する。クラッシュ後の再起動でも状態が失われないようにする（旧Paper版はメモリ上の変数のみで、再起動すると状態が消失するバグがあった）。

- `running`は`SavedData`の**新規作成時**（＝`/start`によるフレッシュなセーブフォルダ生成時、既存データが無い場合の初期値供給）に`true`として初期化する。以降は5.3節の死亡カウントダウン終了時・挑戦終了系ボス討伐時に`false`へ更新される
- **戦犯UUID（`dead`）は`SavedData`に置かない。** 旧Paper版では死亡発生時から後続の記録処理（`deleteRtaWorlds()`/`archive()`）まで値を一時保持する必要があったため`dead`フィールドが存在したが、NeoForge版では死亡イベント発生と同時に5.5節のイベントログへ直接書き込む設計に変更したため、値を跨tick・跨リスタートで保持する必要が無くなった。死亡ハンドラ内のローカル変数で完結する

`SavedData`はワールドのセーブフォルダ内に保存されるファイルであるため、Managerがフォルダごとバックアップ/復元する`/archive`・`/load`の対象に自然に含まれる。`/load`時に`running`・`challengeId`がその時点の状態へ巻き戻るのは意図通り（チェックポイントへ戻るとはそういうことなので）。

**経過時間だけは`SavedData`に置かない。** `SavedData`はチェックポイント復元で過去の値に巻き戻る仕組みであり、そこに経過時間を置くと「チェックポイントから全滅までの間に経過した時間」が`/load`のたびに失われてしまう。経過時間は5.5節の通り、セーブフォルダの外にある挑戦記録ファイル（イベントログ）の最新イベントを基準に管理し、`/load`で巻き戻らないようにする。

#### 経過時間の計測方式（tickではなく実時間）

旧Paper版の`RTATimer`は`runTaskTimer(plugin, 0, 20L)`で「20tickごとに+1」する方式で、サーバーのTPSが低下すると実時間からズレる欠点があった。NeoForge版では実時間ベースに変更する：

- サーバーtickごとに、前回tickからの実経過時間（`System.nanoTime()`による差分。壁時計調整の影響を受けないモノトニッククロックを使う）をメモリ上のカウンタへ加算する。起点は5.5節の挑戦記録ファイルの`lastKnownElapsedTime`
- `running=false`の間は加算しない（サーバーが止まっている間、あるいは挑戦終了後は経過時間が進まない）
- **数秒間隔（例：10秒ごと）で、その時点のカウンタ値を5.5節の挑戦記録ファイルの`lastKnownElapsedTime`へ都度書き込む。** イベント（セーブ/死亡/クリア）発生時にも同じフィールドを更新する。これにより、イベントの間隔がどれだけ空いても、クラッシュ時のロスは書き込み間隔（数秒〜数十秒）程度に収まる

### 5.2 コマンド（hardcore MODレベル、hardcore接続中のみ使用可能）

| コマンド | 権限 | 動作 |
|---|---|---|
| `/archive <name>` | OP | `save-off`→`save-all flush`を実行後、Managerへ`archive-request(name, elapsedTime)`を送信。**`name`が既存アーカイブと重複する場合はManagerに拒否され、OPへエラー表示して終了**（`save-on`のみ実行して中断）。成功時は`archive-complete`受信後に`save-on`で再開。サーバープロセスは止めない。あわせて5.5節のイベントログへ`type: save`（トリガー：実行したOP）を追記する |

`/start`・`/load`はhardcore MODには持たせない（Gateレベルのコマンドとして2.1節に記載。理由：hardcoreサーバーが未起動の状態でも呼び出せる必要があるため）。`/savedata`・`/senpan`も同様にhardcore MODには持たせず、Managerが直接読み取る（2.2節・3.3節。理由：hardcore停止中・他バックエンド接続中でも使えるようにするため）。

一方`/archive`はhardcore MODが直接持つ（Gate/Managerへ中継しない）。`/archive`は稼働中のMinecraftサーバープロセスでしか実行できない操作（オートセーブの一時停止）を必ず伴うため、「hardcoreに接続していなくても実行できる」という利点が実質的に無く、Gate/Manager経由の中継に伴う複雑さ（Manager⇔hardcore MOD間の新規コマンド中継シグナル、受信スレッドでのデッドロック回避、hardcoreに繋いでいない実行者へのメッセージ配送）に見合わないと判断した（2.2節参照）。旧`/rta`（同一プロセス内での複数ワールド間テレポート）は廃止（lobby/hardcoreが別プロセスになったため不要）。

### 5.3 ゲームプレイ仕様

- サーバー起動時のhardcoreモード・難易度HARDは、hardcoreサーバーの`server.properties`の`hardcore=true`（バニラ標準機能、新規ワールド生成時にハードコアモード・難易度HARD固定で生成される）で固定する（NeoForgeにランタイムでの`setHardcore`相当APIが無いため、生成時点の設定で担保する）。**ワールドのシード値は固定しない**——`/start`のたびに`world/`を削除して`server.properties`の`level-seed`を空のまま新規生成させることで、挑戦ごとに異なるシードになる。あらかじめ焼き込んだセーブテンプレートを複製する方式は採らない（Manager側の実装詳細は`architecture-manager.md`参照）
- プレイヤー参加時：参加メッセージは非表示
- **プレイヤー死亡時**（`LivingDeathEvent`、対象がPlayerかつ`running=true`の場合のみ）：
  1. 次tickでスペクテイターへ強制リスポーン（体力/満腹度リセット）。**「次tickで・強制的に」行う理由**：vanillaのハードコアBAN（`server.isHardcore()`時に死亡プレイヤーが`banned-players.json`へ追加される処理）は、`ServerPlayer`の死亡処理自体にではなく、クライアントが死亡画面で「リスポーン」を押した際に送る`PERFORM_RESPAWN`パケットのハンドラ（`ServerGamePacketListenerImpl#handleClientCommand`）側に実装されている。そのため、MOD側が死亡直後（次tick）に`PlayerList#respawn`相当のメソッドをサーバー内部から直接呼んでリスポーンを完了させてしまえば、そのクライアントパケットハンドラ自体を一度も経由せず、BAN分岐に到達しない（仮にクライアントが後から「リスポーン」を押しても、その時点で既に体力が回復済みのため同ハンドラ冒頭の早期returnで弾かれる）。旧Paper版（`hardcore/`、Bukkit APIの`Player#spigot().respawn()`）・現行NeoForge実装（`hardcore-together-neoforge`の`DeathCountdown.kt`、`PlayerList#respawn`直接呼び出し）とも、この同じ回避策を採用している
  2. 既にカウントダウン中でなければ、5.5節のイベントログへ`type: death`のイベントを追記する。死亡したプレイヤー（UUID・名前）と、そのときのキルログ（`LivingDeathEvent`の`DamageSource`から得られる死亡メッセージ相当の情報）を含める。`/archive`の実行有無に関係なく必ず行う
  3. 全員に死亡演出（タイトル＋効果音）
  4. 10秒カウントダウン演出。終了時、まだSURVIVAL状態の全プレイヤーをSPECTATORへ強制送還し、`running=false`・理由「全滅」としてSavedDataに保存
  - 仕様：1人でも死亡すればパーティ全体の挑戦終了（個々のプレイヤーが生存していても終了する）
- **指定ボスMob討伐時**（`LivingDeathEvent`、対象が設定済みボスリストに含まれるMobかつ`running=true`の場合）：ボスごとに「チェックポイント系」「挑戦終了系」のどちらかを設定ファイルで指定する。共通処理は以下：
  1. `save-off`→`save-all flush`
  2. Managerへ`archive-request(elapsedTime)`を送信（`name`は省略する）。名前はプレイヤー入力が無いためManager側が討伐時点相当の日時から自動生成する（3.2節参照。衝突時はManager側で連番を付与するため、ここでの失敗は基本的に発生しない）
  3. `archive-complete`受信で`save-on`。`archive-complete`の`name`が、Manager側で実際に採用された（連番付与済みの場合はそれを含む）アーカイブ名
  4. 5.5節のイベントログへ、討伐したボスのMob IDをトリガーとして含むイベントを追記する。`archiveName`には手順3で受け取った`archive-complete`の`name`を使う（3.2節）

  この後の扱いがカテゴリごとに分岐する：
  - **チェックポイント系**：`running`はfalseにしない。タイマーも継続する（挑戦は続行）。追記するイベントは`type: save`
  - **挑戦終了系**：`running=false`・理由「討伐クリア:<ボス名>」としてSavedDataに保存。タイマーはリセットではなく停止として記録（旧Paper版のエンダードラゴン討伐時と同じ扱い）。追記するイベントは`type: clear`

  - 対象ボスの分類は5.4節の設定ファイルで管理する
  - 手動`/archive`コマンドの場合も同様に5.5節へ`type: save`のイベントを追記する。トリガーがMobではなく実行したOPになる点のみ異なる（内部のアーカイブ処理自体は共通）
- 挑戦終了後にlobbyへ戻るのは手動（`/lobby`を自分で打つ）。自動一括転送は行わない

### 5.4 ボス設定ファイル

チェックポイント系・挑戦終了系のどちらも、**複数指定を前提としたリスト**として設定ファイルで管理する（挑戦終了系も「念のため複数」を許容し、1体固定にはしない）。

```toml
[bosses]
# 討伐すると自動アーカイブするが、挑戦は継続するボス（複数可）
checkpoint = [
    "twilightforest:naga",
    "twilightforest:lich",
    "twilightforest:hydra",
    "twilightforest:knight_phantom"
]

# 討伐すると自動アーカイブし、running=falseにして挑戦終了とするボス（複数可）
clear = [
    "twilightforest:ur_ghast",
    "twilightforest:alpha_yeti"
]
```

- 値はMobのレジストリID（`modid:path`形式）の文字列配列
- `checkpoint`・`clear`いずれも0件〜複数件を許容する。同一IDを両方に重複登録した場合は`clear`側を優先する（挑戦終了を取りこぼさないため）
- サーバー起動時に読み込み、`LivingDeathEvent`ハンドラ内で対象EntityTypeのIDをこの2リストと照合して分岐する
- デフォルト値は黄昏の森（Twilight Forest）のボス群を想定した上記の例だが、具体的にどのボスをどちらに分類するかは10節の未決事項として最終確認が必要

### 5.5 挑戦記録データ（challengeIdごとのイベントログ）

`/savedata`・`/senpan`が参照する記録、および経過時間の継続計算は、すべてこの仕組みに一本化する。アーカイブ機構（Managerの`archive-request`）とは独立しており、`/archive`を一度も実行しない挑戦であっても、死亡・クリアの結果は必ず記録される。

**ファイル配置**

- **`challengeId`ごとに1ファイル**を作成する（単一ファイルへの集約はしない）。例：`records/<challengeId>.json`
- 配置場所はhardcore MODの設定ファイル（`storage.recordsDir`、デフォルト値`records`）で指定する。相対パスはhardcoreサーバーの作業ディレクトリ基準（デフォルトなら`<hardcoreサーバー作業ディレクトリ>/records/<challengeId>.json`、`world/`と同階層）、絶対パスはそのまま使う。Managerが`/start`時に削除するのは`world/`のみなので、この場所は影響を受けない。archive/（Manager管理、hardcoreサーバーの作業ディレクトリの外）とは別の場所である点に注意（11節参照）
- **書き込み**はhardcore MODがサーバープロセス内で直接行うローカルファイルI/Oで完結し、Managerを経由しない（6節の`archive-request`/`archive-complete`とは無関係）
- **読み取り**（`/savedata`・`/senpan`、2.2節）はhardcore MODを経由せず、Managerが同一コンテナ前提でこのファイルを直接読み取る（3.3節）。書き込み側と読み取り側で経路が異なる点に注意
- **設定を分ける2つのプロセスが同じ場所を見る必要がある**：`storage.recordsDir`はhardcore MOD自身の設定ファイルの値だが、Managerも同じ場所を読みに行く（3.3節）ため、Manager側の設定（例：`config.yml`の対応する値）と必ず一致させる必要がある。値を変更する場合は両方を同時に更新すること

**ファイル構造**

```json
{
  "challengeId": "a1b2c3d4-...",
  "lastKnownElapsedTime": 1500,
  "events": [
    { "...": "イベントは種別ごとに以下のいずれかの形" }
  ]
}
```

`elapsedTime`はすべて**long型（秒数）**で保持する（`"01:23:45"`のような整形済み文字列では保存しない。表示が必要な箇所でのみ都度フォーマットする）。`lastKnownElapsedTime`はルート直下のフィールドで、5.1節の定期永続化（数秒間隔）とイベント発生時の両方で更新される、経過時間の最新値。

**命名について**：ルート直下の値を`events[i].elapsedTime`と同じ`elapsedTime`にしなかったのは、両者が別の意味を持つため。`events[i].elapsedTime`は各イベント発生時点の確定した歴史的記録（書いたら変わらない）で、`lastKnownElapsedTime`は定期書き込みとイベント発生の両方で更新され続ける値であり、通常は直近のイベントより進んだ値になる（イベント間もサーバーは動き続け、10秒ごとにこの値だけ更新されるため）。`lastKnown`（最後に確認できた）という接頭辞で、「最大で心拍間隔（10秒）分古い可能性はあるが、クラッシュ復旧時に使える最良の値」という位置づけを明示している。

全イベント共通のフィールド：`type`（`save` | `death` | `clear`）、`elapsedTime`（そのイベント時点の経過時間、秒数）、`timestamp`（発生した壁時計日時、表示・ソート用でelapsedTime計算には使わない）。

- **`save`**（`/archive`実行時、またはチェックポイント系ボス討伐時）：
  ```json
  { "type": "save", "elapsedTime": 600, "timestamp": "2026-07-18T12:00:00Z",
    "archiveName": "2026-07-18T12-00-00",
    "trigger": { "kind": "boss", "mobId": "twilightforest:naga" } }
  ```
  手動`/archive`の場合は`trigger`が`{ "kind": "manual", "player": "<実行したOPのUUID>" }`になる
- **`death`**（プレイヤー死亡時）：
  ```json
  { "type": "death", "elapsedTime": 900, "timestamp": "2026-07-18T12:05:00Z",
    "deadPlayer": { "uuid": "...", "name": "Steve" },
    "killLog": "Steve was slain by Zombie" }
  ```
  `killLog`は`LivingDeathEvent`の`DamageSource`から得られるバニラの死亡メッセージ相当の文字列（将来的に攻撃者エンティティ種別等を構造化して追加してもよい）
- **`clear`**（挑戦終了系ボス討伐時）：
  ```json
  { "type": "clear", "elapsedTime": 1500, "timestamp": "2026-07-18T12:45:00Z",
    "trigger": { "kind": "boss", "mobId": "twilightforest:ur_ghast" } }
  ```

**経過時間の継続（`/load`時の巻き戻り対策）**

- サーバー起動時、現在の`challengeId`（`SavedData`から取得）に対応する`records/<challengeId>.json`が存在すれば、ルート直下の`lastKnownElapsedTime`を読み取り、そこから実時間加算を再開する
- ファイルが存在しない（`/start`直後で初めての場合）場合は`lastKnownElapsedTime=0`から開始する
- これにより、`/load`でセーブフォルダ（＝ワールド状態）が古いチェックポイントに巻き戻っても、経過時間は巻き戻らず、直前まで進んでいた時点から継続する。イベントの間隔が空いていても、定期永続化のおかげで巻き戻り基準点は最新に近い値を保つ

**コマンドからの参照**

- `/savedata`：全`challengeId`のファイルを横断して、`save`/`death`/`clear`イベントの一覧を表示
- `/senpan list|count`：全`challengeId`のファイルから`type: death`のイベントを集計し、`deadPlayer`ごとの一覧・回数を表示

（いずれも3.3節の通りManagerが直接ファイルを読み取り、Gateがそれを問い合わせて実現する。hardcoreサーバーの起動有無に関わらず参照可能）

**例（ご指摘の例に対応）**

```
/start                          → challengeId=A発行。records/A.json新規作成、lastKnownElapsedTime=0、events=[]
チェックポイント系ボス討伐        → events: [ save(elapsedTime=T1, trigger=boss:naga) ]、lastKnownElapsedTime=T1
全滅                            → events: [ save(T1), death(elapsedTime=T2, deadPlayer=Steve, killLog=...) ]、lastKnownElapsedTime=T2
/load save1                      → ワールドはT1時点に巻き戻るが、経過時間はrecords/A.jsonのlastKnownElapsedTime(T2)から再開
討伐クリア                       → events: [ save(T1), death(T2), clear(elapsedTime=T2+その後の経過, trigger=boss:ur_ghast) ]、lastKnownElapsedTimeも同値に更新
```

`records/A.json`には`save`・`death`・`clear`の3イベントがすべて時系列で残り、全滅の事実・時刻・キルログを含めて挑戦Aの経緯を後から完全に追跡できる。

## 6. MOD⇔Manager間シグナル

詳細なフィールド定義・具体例・シーケンス図は`docs/protocol-mod-manager.md`を参照。

| シグナル | 方向 | 発生タイミング | ペイロード |
|---|---|---|---|
| `ready` | hardcore MOD → Manager | `ServerStartedEvent`発火時 | `running`（起動直後の`running`値。Managerの3.1節キャッシュの初期値として使う） |
| `running-changed` | hardcore MOD → Manager | `running`の値が変化するたび（新規作成時の`true`初期化、全滅/挑戦終了系ボス討伐による`false`化） | `running`（変化後の値） |
| `archive-request` | hardcore MOD → Manager | `/archive <name>`実行時（`save-off`済み） | `name`（**任意。省略時はManagerが自動生成する**。手動/自動を区別する専用フィールドは無く、`name`の有無だけで3.2節の挙動が決まる）, `elapsedTime`（long、秒数） |
| `archive-complete` | Manager → hardcore MOD | ファイルコピー完了時 | `name`（Managerが実際に採用した最終的なアーカイブ名。MODはこれを受けて`save-on`を実行し、`archive-request`で`name`を省略していた場合は5.5節`archiveName`にもこの値を使う） |

`archive-request`から`deadPlayerUUID`は削除した。死亡記録は5.5節のイベントログに完全移行しており、セーブ（チェックポイント）イベントに死亡プレイヤー情報を含める理由が無いため。

`archive-request`から`createdAt`も削除した（Manager自身が処理時点の現在時刻から生成するため、MODが送る必要が無い）。同様に、手動/自動を区別する`origin`のようなフィールドは設けず、`name`を**任意**フィールドにするだけに留めた（省略時はManagerが自動生成する）。理由：ボス討伐時の名前・作成日時の生成ロジック（タイムスタンプの整形）をMOD側に持たせる必要が無く、実際にファイルコピーを行うManager側で完結させた方がシンプルになる上、`name`の有無自体が「手動か自動か」を過不足なく表しており、それを表現する専用フィールドを別途持つのは冗長だったため。

### 6.1 通信方式：永続TCPソケット＋NDJSON

Gateには`GateService`という組み込みのgRPC/HTTP APIがあるが、これはGateプロキシ自体の管理（プレイヤー一覧・サーバー登録など）専用の固定APIであり、`archive-request`のようなHardcore Together独自のシグナルを流すための汎用の仕組みではない。そのため、MOD⇔Manager間には専用の軽量な通信路を別途用意する。

- **トランスポート**：TCPソケット。Managerが`127.0.0.1`限定でリッスンする（1節の「Managerがhardcoreを同一コンテナ内の子プロセスとして起動する」前提に基づく。同一プロセスツリー内・同一コンテナ内通信のためTLS/認証は不要と判断）。ポート番号はManagerの設定ファイルで指定（例：`signalPort`）
- **接続方向**：hardcore MOD側が接続しにいく。Managerは常駐プロセス、hardcore MODは`/start`・`/load`のたびにプロセスごと生成・破棄されるため、短命な側（MOD）が長命な側（Manager）へ接続する方が自然
- **接続タイミング**：MODは`ServerStartedEvent`発火時に接続し、直後に`ready`を送る。接続失敗時は数回リトライ＋バックオフして諦める（ログ出力）
- **メッセージ形式**：改行区切りJSON（NDJSON）。1メッセージ＝1行のJSONオブジェクト。`type`フィールドで本節の表のどのシグナルかを判別する

```json
{"type":"ready","running":true}
{"type":"running-changed","running":false}
{"type":"archive-request","elapsedTime":600}
{"type":"archive-request","name":"save1","elapsedTime":600}
{"type":"archive-complete","name":"2026-07-18T12-00-00"}
```

- **`archive-request`の同期待ち**：MODは送信後、次に届く`archive-complete`を受信するまで待ってから`save-on`を実行する（3.2節の手順）。1つのTCP接続上で`archive-request`は常に1件ずつ・同期的に処理される（MOD側が`archive-complete`を待ってから次の操作へ進む設計のため）ので、`name`を送っていない場合でも取り違えは起きない。`name`を省略していた場合、`archive-complete`の`name`がManagerの生成した実際のアーカイブ名であり、MODは以後この値を使う（5.5節`archiveName`等）
- **接続断の扱い**：接続が切れた場合、Managerは自身の`os/exec`ハンドルでプロセスの生死を確認する。**プロセスが生きているのに接続だけが切れている場合**は状態を「不明」とみなし、`running`は安全側（`true`扱い、`/start`・`/load`拒否）にする。**プロセス自体が終了している場合**は「不明」にはせず、直前に永続化していた`running`値をそのまま使う（2.1節「プロセス状態と`running`の永続化」）——プロセスが無い間は新しい`running-changed`が届きようがないので、それを「不明」として扱う理由が無い
- protobuf/ConnectRPCのような型付けIDLは採用しない。シグナル数が少なく（4種）、既存の技術スタックに揃える必要性より、依存を増やさないシンプルさを優先した

## 7. Gate⇔Manager間シグナル

詳細なフィールド定義・具体例・シーケンス図は`docs/protocol-gate-manager.md`を参照。

GateとManagerは別プロセスであるため、2節のGateコマンドを実行するには、Gateが実際の処理をManagerへ依頼し、結果を受け取る必要がある。6節のMOD⇔Manager間シグナルと同様に、**永続TCPソケット＋NDJSON**で統一する（技術スタックを増やさないため）。

### 7.1 通信方式

- トランスポート：TCPソケット。Gateが起動時にManagerへ接続しにいく（両者とも常駐プロセスのため接続方向はどちらでもよいが、Managerを「サービス提供側」として一貫させるため、6節と同じくクライアント側（Gate）が接続する方式に揃える）。接続失敗時は数回リトライ＋バックオフする
- Gate・Manager間はDocker network内の別コンテナ間通信になりうるため、6節の`127.0.0.1`限定とは異なり、Managerは待受アドレスを設定可能にする（例：`0.0.0.0:<gateApiPort>`）。ただしDockerのポート公開設定でホスト外へは公開せず、Docker network内のみで到達可能とすることで、6節の「同一ホスト内通信のためTLS/認証は不要」と同等の信頼境界を維持する
- メッセージ形式：NDJSON。`type`フィールドで下表のどのシグナルかを判別する

### 7.2 シグナル一覧

| シグナル | 方向 | 発生タイミング | ペイロード |
|---|---|---|---|
| `state-query` / `state-response` | Gate → Manager / Manager → Gate | Gateが hardcoreの現在状態を知りたいタイミング（`/rta`実行時など）で都度送信する同期的な問い合わせ | request: なし。response: `state`（`stopped`\|`starting`\|`ready`\|`stopping`）, `running`（`true`\|`false`\|`unknown`） |
| `hardcore-ready` | Manager → Gate | `/start [clean]`・`/load`の一連処理が完了し、hardcoreが準備完了になった時（1回限りの完了通知） | なし |
| `start` | Gate → Manager | `/start [clean]`受理時 | `clean`（bool）, `requestedBy`（実行者名） |
| `load` | Gate → Manager | `/load <name\|latest> [force]`受理時 | `name`, `force`（bool）, `requestedBy` |
| `deactivate` | Gate → Manager | `/deactivate`受理時 | `requestedBy` |
| `start-rejected` / `load-rejected` | Manager → Gate | プロセス状態チェック・`running`チェック・アーカイブ存在チェック等で拒否した場合 | `reason`（文字列、例：「既に起動しています」「挑戦が進行中です」「アーカイブ`<name>`は存在しません」） |
| `deactivate-rejected` | Manager → Gate | プロセス状態チェックで拒否した場合 | `reason`（「既に停止しています」または遷移中「処理中です」） |
| `evacuate-request` | Manager → Gate | `start`（`clean=true`）/`load`/`deactivate`受理後、プロセスが起動中で止める前（2.3節） | `reason`（`reset`\|`force-reset`\|`deactivate`、2.1節のメッセージ文言切り替えに使用） |
| `evacuate-complete` | Gate → Manager | 退避（hardcore接続中の全プレイヤーをlobbyへ強制転送）完了時 | なし |
| `deactivate-complete` | Manager → Gate | `deactivate`によるプロセス停止が完了した時（1回限りの完了通知） | なし |
| `savedata-query` / `savedata-response` | Gate → Manager / Manager → Gate | `/savedata`実行時 | request: なし。response: `events`（3.3節の集計結果） |
| `senpan-query` / `senpan-response` | Gate → Manager / Manager → Gate | `/senpan list\|count`実行時 | request: `mode`（`list`\|`count`）。response: 集計結果 |

`hardcore-ready`受信をもって、Gateは2.1節の「その時点でlobbyに接続している全プレイヤーを自動でhardcoreへ接続」を実行する（旧仕様の`ready`受信と同じ役割。`clean`無しの`/start`経由の起動でも発生する）。`deactivate-complete`受信をもって、Gateはコマンド実行者へ「サーバーを停止しました」と表示する。`state-query`/`state-response`は継続的なキャッシュ同期ではなく、Gateが必要な時に都度問い合わせる方式にしている（`/start`・`/load`はどのみちManager側のチェック・排他ロックを経由するため、Gate側にキャッシュを持たせても問い合わせを省略できず、キャッシュのズレというリスクだけが残るため。詳細は`docs/protocol-gate-manager.md` 6節参照）。

### 7.3 `/start clean`・`/load`実行フロー（Gate/Manager間のやり取り）

退避を伴う、破壊的な系統（`/start clean`・`force`が効く`/load`）のフロー。

1. Gate：コマンド受理・権限チェック
2. Gate → Manager：`start`（`clean:true`）または`load`送信
3. Manager：（`load`の場合のみ）`running`チェック・アーカイブ存在チェック → 拒否ならManager → Gate：`start-rejected`/`load-rejected`を返して終了（`start`は`clean:true`の場合、`running`に関するチェックは無い。ただし遷移中〈`starting`/`stopping`〉であれば`start-rejected`「処理中です」で拒否されうる、2.1節）
4. Manager → Gate：`evacuate-request`（その時点でプロセスが起動中の場合のみ。停止中なら手順4〜5は省略し手順6へ）
5. Gate：hardcore接続中の全プレイヤーをlobbyへ強制転送 → Gate → Manager：`evacuate-complete`
6. Manager：（プロセスが起動中だったなら）プロセス停止 → （`load`なら復元、`start clean`ならワールド削除）→ プロセス起動
7. hardcore MOD起動 → Manager が`ready`受信（6節）
8. Manager → Gate：`hardcore-ready`
9. Gate：その時点でlobbyにいる全プレイヤーをhardcoreへ自動接続

この一連の流れにより、退避（プロキシ固有の操作）はGateが行い、プロセス制御・ファイル操作（Manager固有の操作）はManagerが行うという責務分担を保ったまま、旧仕様と同じユーザー体験（2.1節）を実現する。

### 7.4 `/start`（`clean`無し）・`/deactivate`実行フロー（Gate/Manager間のやり取り）

詳細なシーケンス図は`docs/protocol-gate-manager.md` 4節を参照。ワールドに触れない、非破壊的な系統のフロー。

`/start`（`clean`無し。対象状態は常にプロセス停止中、2.1節の①・④・⑥。プロセスが既に起動中/遷移中の場合はこの手順に入らず即座に`start-rejected`）：

1. Gate：コマンド受理・権限チェック
2. Gate → Manager：`start`（`clean:false`）送信
3. Manager：プロセス状態チェック（起動中〈`ready`〉なら「既に起動しています」、遷移中〈`starting`/`stopping`〉なら「処理中です」で拒否） → 拒否ならManager → Gate：`start-rejected`を返して終了。`running`・`world/`の存在はいずれも判定に使わない
4. Manager：プロセス起動（`world/`が無ければ新規作成、あれば中身に一切触れない。2.1節）
5. hardcore MOD起動 → Managerが`ready`受信（6節）
6. Manager → Gate：`hardcore-ready`
7. Gate：その時点でlobbyにいる全プレイヤーをhardcoreへ自動接続

`/deactivate`（対象状態はプロセス起動中でありうる、2.1節の③・⑤）：

1. Gate：コマンド受理・権限チェック
2. Gate → Manager：`deactivate`送信
3. Manager：プロセス状態チェック → 拒否ならManager → Gate：`deactivate-rejected`を返して終了
4. Manager → Gate：`evacuate-request`
5. Gate：hardcore接続中の全プレイヤーをlobbyへ強制転送 → Gate → Manager：`evacuate-complete`
6. Manager：プロセス正常停止
7. Manager → Gate：`deactivate-complete`
8. Gate：実行者へ「サーバーを停止しました」と表示

## 8. コマンド一覧（全体まとめ）

（実行元はユーザー視点の説明であり、内部実装上はGateがManagerへ処理を委譲する場合がある。2節・3節・7節参照）

| コマンド | 実行元 | 権限 |
|---|---|---|
| `/rta` | Gate（どこからでも） | 誰でも |
| `/lobby` | Gate（どこからでも） | 誰でも |
| `/start` | Gate（どこからでも） | OP |
| `/start clean` | Gate（どこからでも） | OP |
| `/load <name>` | Gate（どこからでも） | OP |
| `/load <name> force` | Gate（どこからでも） | OP |
| `/load latest` | Gate（どこからでも） | OP |
| `/load latest force` | Gate（どこからでも） | OP |
| `/deactivate` | Gate（どこからでも） | OP |
| `/server` | Gate | 権限保有者のみ表示 |
| `/checkpoint` 系 | lobby MOD | 誰でも／一部OP相当権限 |
| `/archive <name>` | hardcore MOD（hardcore接続中のみ） | OP |
| `/savedata` | Gate（どこからでも。hardcore停止中も可、Manager経由でrecords/を読み取り） | 誰でも |
| `/senpan list\|count` | Gate（どこからでも。hardcore停止中も可、Manager経由でrecords/を読み取り） | 誰でも |

## 9. 決定ログ

- ロビーもmoddedにする（Vanilla/Paperとの混在は不採用）
- `/archive`コマンド自体は自動化せず、手動のまま維持する。ただし指定ボスMob討伐時は別途イベント経由で自動アーカイブが走る
- ボスMobは設定ファイルで「チェックポイント系（討伐しても挑戦継続）」「挑戦終了系（討伐でrunning=falseにしてクリア扱い）」のどちらかに分類できるようにする。挑戦を明示的に終了させるトリガーは「プレイヤー死亡（全滅）」と「挑戦終了系ボスの討伐」の2種類になる
- チェックポイント系・挑戦終了系はどちらも複数指定可能なリストとしてconfigで管理する（挑戦終了系も1体固定にせず、念のため複数を許容する）
- サーバー起動・ワールドバックアップの責務はGateに統合する（別プロセスのSupervisorは作らない）（→ 後にManager分離を採用し、本方針は見直した。本節末尾の追記を参照）
- 参加コマンドは`/join`ではなく`/rta`
- `/start`・`/load`実行後、`ready`受信時点でlobbyにいる全プレイヤーが自動でhardcoreへ接続される(スナップショットは取らない)
- 挑戦終了後のlobby帰還は手動（自動一括転送はしない）
- `/archive`はサーバーを止めずに実行可能。ただし`save-all flush`だけでは不十分で、`save-off`→コピー→`save-on`のブラケットが必須
- `/load latest`で、全アーカイブの中から`createdAt`が最も新しいものを自動選択して復元できるようにする
- 挑戦記録データ（`/savedata`・`/senpan`用）はアーカイブ機構から独立させ、セーブフォルダ外に**`challengeId`ごとの個別JSONファイル**としてMODが直接追記する。`/archive`の実行有無に関わらず、イベント発生のたびに必ず記録される
- 経過時間はtickカウントではなく実時間（tick間の実経過時間の積算）で計測する
- 挑戦ID（`challengeId`）を導入し、`/start`で新規発行・0秒から開始する。`challengeId`自体は`SavedData`経由で`/load`と一緒に巻き戻ってよいが、経過時間は`SavedData`に置かず、その`challengeId`のイベントログファイルにある**`lastKnownElapsedTime`**（数秒間隔で定期永続化される値）から継続計算する（`SavedData`内に置くと、チェックポイント復元のたびに全滅までの時間が失われるため）
- 記録イベントは`save`（`/archive`実行時・チェックポイント系ボス討伐時、トリガーmob/OPを含む）・`death`（プレイヤー死亡時、死亡者とキルログを含む）・`clear`（挑戦終了系ボス討伐時、トリガーmobを含む）の3種類。1つの`challengeId`のファイルに時系列で追記されていく
- `running`は`SavedData`の新規作成時（`/start`によるフレッシュなセーブフォルダ生成時）に`true`で初期化する
- 戦犯UUID（`dead`）は`SavedData`に保存しない。死亡イベント発生と同時に5.5節のイベントログへ直接書き込むため、値を跨tick・跨リスタートで保持する必要が無い
- 全体レビューで見つかった以下の抜け・矛盾を修正：
  - `/start`・`/load`はhardcoreの`running`が`true`（挑戦進行中）の場合は拒否する。Managerは`ready`/`running-changed`シグナルで`running`値をキャッシュして判定する
  - `archive-request`から`deadPlayerUUID`を削除（死亡記録は5.5節のイベントログに完全移行済みのため不要だった）
  - 経過時間の永続化は「イベント発生時のみ」ではなく、数秒間隔の定期書き込み（`lastKnownElapsedTime`）に戻す。イベント間隔が空いてもクラッシュ時のロスを書き込み間隔程度に抑える
  - `/archive <name>`は名前の重複を拒否する。ボス討伐による自動アーカイブの名前は`<討伐時点の日時>`とし、衝突時はManager側で連番を付与する
  - `/load <name>`で該当アーカイブが存在しない場合はエラーメッセージを返す
  - `elapsedTime`は整形済み文字列ではなくlong（秒数）で保持する。表示時にのみフォーマットする
  - 全滅カウントダウン中の2人目以降の死亡イベントはログに記録しない（最初の死亡のみを戦犯として扱う、現行仕様のまま）
  - 死亡時にManagerへの`archive-request`は送らない（死亡直後のワールド状態そのものは自動バックアップしない、現行仕様のまま）
- Gate・lobby・hardcoreは同一ホスト上で動作する前提を明記。`archive/<name>/`はGate自身が管理するディレクトリ（設定ファイルでパス指定）で、hardcoreサーバーの作業ディレクトリとは別の場所に置く（→ Manager分離に伴い実装主体はManagerへ移管。本節末尾の追記を参照）
- `records/<challengeId>.json`の配置場所を確定：hardcoreサーバーの作業ディレクトリ内、`world/`と同階層の`records/`フォルダ（Manager管理の`archive/`とは別の場所）
- 全パス情報を11節「ディレクトリ構成」に集約した
- `/start force`・`/load <name> force`・`/load latest force`を追加。`running=true`（挑戦進行中）による拒否チェックのみを免除し、進行中の挑戦を強制的に中断・破棄できるようにする。アーカイブ実行中の排他制御など他の安全機構は`force`でも免除しない
- プロジェクト名を確定：hardcoreサーバーのMODは**Hardcore Together**（連帯責任を表す。特定MODに依存しない汎用フレームワークのため固有名詞は含めない）、lobbyサーバーのMODは**Parkour Lobby**（チェックポイントはパルクールコースの中間セーブという位置づけ）、プロキシは**Hardcore Together Gate**
- 両MODとも**サーバーサイドのみで完結**させる。クライアント側へのMOD導入は不要とし、カスタム`Item`/`Block`登録は行わない。Parkour Lobbyのチェックポイント用アイテム・看板はバニラのアイテム/看板ブロックに`minecraft:custom_data`でフラグを付与する方式に変更した
- MOD⇔Gate間シグナルの通信方式を確定：永続TCPソケット＋NDJSON（改行区切りJSON）。Gateの組み込みAPI（`GateService`）はプロキシ管理専用の固定APIでHardcore Together独自シグナルには使えないため、専用の軽量ソケットを別途用意する。protobuf/ConnectRPCは今回の規模（シグナル4種）にはオーバースペックと判断し不採用（→ 後にMOD⇔Manager間シグナルへ改称。本節末尾の追記を参照）
- 【修正】`/savedata`・`/senpan`を、hardcore MODレベル（hardcore接続中のみ実行可）からGateレベル（どこからでも実行可能、2.6節）へ変更した。Gateが`records/<challengeId>.json`を（Gate・hardcoreが同一ホスト上にあるという1節の前提のもと）直接読み取ることで実現し、hardcore停止中でも閲覧可能にした（→ 実装主体はGateからManagerへ移管。本節末尾の追記を参照）
- 【検討の上、不採用】`/archive`も同様にGateレベルへ移し、Gateがhardcore MODへ新設シグナルを中継する案を一時検討したが、不採用とした。`/archive`は稼働中のMinecraftサーバープロセスでしか実行できない操作（オートセーブの一時停止）を必ず伴うため「hardcoreに接続していなくても実行できる」利点が実質的に無く、中継のために必要になる複雑さ（新規シグナル2種、Gate接続の受信スレッドで直接処理するとブロッキングによりデッドロックしうる問題への対処、hardcoreに繋いでいない実行者へのメッセージ配送）に見合わなかった。`/archive`は引き続きhardcore MODレベルの直接コマンドのまま維持する（5.2節）
- `records/`の配置パスをhardcore MODの設定ファイル（`storage.recordsDir`、デフォルト`records`）から指定できるようにした。理由：このパスはhardcore MOD（書き込み）とManager（`/savedata`・`/senpan`用の読み取り、3.3節）という2つの別プロセスが同じ場所を見に行く必要がある、プロセスをまたいで読まれる唯一のデータであるため。値を変える場合はManager側の設定も同時に更新する必要がある（5.5節）
- 【アーキテクチャ変更】プロキシ機能（Gate）とサーバー管理機能（プロセスライフサイクル・アーカイブ・records読み取り）を別プロセス「Manager」に分割した。**本決定は上記の「サーバー起動・ワールドバックアップの責務はGateに統合する（別プロセスのSupervisorは作らない）」および「Gate・lobby・hardcoreは同一ホスト上で動作する前提...Gate自身が管理するディレクトリ」「`/savedata`・`/senpan`をGateレベルへ変更した」を上書きする。**
  - 理由：Gateは本来Minecraftプロトコルのルーティングを行うプロキシであり、OSプロセス管理やファイルシステム操作はプロキシ本来の責務ではないため（単一責任の観点）。Docker運用を想定した場合、この分割によりGateコンテナは完全にファイルシステム非依存（ネットワークのみ）にできるという副次的な利点もある
  - プロセス管理とアーカイブ実行の排他制御（旧決定log参照）は、Manager内部に閉じたまま維持できる（Manager側に「プロセス制御」と「アーカイブ実行」を両方持たせ、Mutexで排他するため）。この点は分離前と変わらない
  - Manager・hardcoreサーバーは`os/exec`の親子プロセス関係のため同一コンテナに同居させる。ManagerがDocker APIを介して別コンテナのhardcoreを制御する案（`docker.sock`のマウントが必要）も検討したが、Docker socketの共有はホストに対するroot相当の権限をManagerに渡すことになりセキュリティ上のリスクが大きく、得られる利点（コンテナの完全分離）に見合わないため不採用とした
  - この変更に伴い、`/savedata`・`/senpan`のrecords直接読み取り、アーカイブ実行、MOD⇔Manager間シグナル受信は全てGateからManagerへ移管した（3節）。GateとManager間は新設のシグナル（7節）で通信する
  - **hardcore MOD・lobby MOD側のプロトコル・実装に変更は無い**：hardcore MODは引き続き同じTCP+NDJSONプロトコルで、同じ「同一ホスト（コンテナ）内のローカルアドレス・`signalPort`」へ接続するだけであり、接続先で待ち受けているのがGateかManagerかはMOD側の実装からは区別されない（6.1節）。旧仕様書5節（MOD⇔Gate間シグナル）は名称を「MOD⇔Manager間シグナル」に変更したのみで、シグナルの種類・ペイロード・プロトコル形式（NDJSON）はすべて維持している
- 【修正】Gate⇔Manager間の`state`シグナルを、push型（状態変化のたび・接続確立直後にManagerから送信）から、Gateが必要な時に都度問い合わせる`state-query`/`state-response`へ変更した。理由：`/start`・`/load`はいずれもManager側の`running`チェック・アーカイブ存在チェック・排他ロックを経由する必要があり、Gate側のキャッシュがあっても問い合わせを省略できていなかった（キャッシュは「早期に弾く」以上の役割を果たさない）一方、push漏れや再接続直後の未受信によるキャッシュのズレというリスクだけが残っていたため。`/rta`がキャッシュに依存せず誤った状態で接続を試みても、`failoverOnUnexpectedServerDisconnect`（`architecture.md` 0.4節）により自動的にlobbyへフォールバックされるため実害は無い。ただし`/start`・`/load`完了時にGateが能動的に問い合わせるタイミングを持たない「lobbyプレイヤーの自動転送」だけは、Manager発の1回限りの完了通知`hardcore-ready`として別途残した（詳細は`docs/protocol-gate-manager.md` 6節）
- 【修正】5.3節のhardcoreモード・難易度HARD固定方式を訂正：当初案の「Managerが事前に焼き込んだセーブテンプレートをコピーする」方式を廃止し、**hardcoreサーバーの`server.properties`の`hardcore=true`（バニラ標準機能）で固定し、ワールドのシード自体は`/start`のたびに新規生成（`level-seed`を空のまま）してランダムにやり直す**方式に変更した。理由：ハードコアモードの固定に必要なのは「新規ワールド生成時点でのフラグ設定」だけであり、バニラの`server.properties`がその機能を標準で提供しているため、テンプレートワールドを事前に用意・複製する仕組みは不要かつ「毎回同じシードで挑戦する」という望ましくない副作用を生んでいた。Manager側の実装詳細（`server.properties`の値保証等）は`architecture-manager.md`参照
- 【修正→さらに簡略化】`archive-request`の名前重複時の挙動（手動→拒否／ボス自動→連番付与）は仕様書上、発生源（手動か自動か）で分けて定義していたが、Managerがどちらのケースかを判別する手段がプロトコル上に存在しないという抜けがあった。当初は`origin`（`"manual"` | `"auto"`）フィールドを追加してこれを解消したが、その後さらに見直し、**`origin`フィールド自体を廃止**した。理由：`archive-request`の`name`・`createdAt`の生成元をMODからManagerへ移した結果（`name`は任意フィールドとなり、省略時はManager自身が処理時点の現在時刻から自動生成、`createdAt`は常にManager生成でプロトコルから削除）、「手動か自動か」は`name`を送ったかどうかだけで一意に決まるようになったため、それを重ねて表現する`origin`は冗長だった。最終的な規約は「`name`を送った場合は手動＝重複時に拒否、省略した場合は自動＝Manager生成＋重複時に連番付与」という、`name`の有無だけに基づくシンプルな分岐になった（3.2節・6節）。この変更に伴い、`name`を省略していた場合はMOD側が最終的なアーカイブ名を事前に知り得なくなるため、Managerが採用した実際の名前は`archive-complete`の`name`で通知し、MODは5.5節のイベントログ（`archiveName`）にこの値を使う
- 【追加】`/start`にワールドを触らず起動するだけの挙動を持たせ、破壊的な再生成は`/start clean`に分離した。あわせて`/deactivate`を新設し、`running`の未確定値の扱いを見直した。
  - 理由：当初案には、一度もhardcoreを起動したことが無い状態からでも`/start`が永遠に成功しないデッドロックがあった。Managerの`running`キャッシュは、Manager起動直後やhardcoreとの接続が確立していない間は安全側に倒して`true`（進行中）扱いにする設計だったが、`/start`・`/load`はこのキャッシュが`true`の間は常に拒否される。一方、キャッシュを`true`から書き換えられるのはhardcore自身が起動して`ready`を送ってきた場合のみであり、そのhardcoreを起動させる`/start`自体が上記の理由で常に拒否される、という循環になっていた。「一度も起動していない」（安全に許可してよい）状態と、「進行中で接続が切れているだけかもしれない」（危険側）状態を、同じ`unknown`＝`true`扱いで区別できていなかったことが根本原因。**Manager自身は`os/exec`によるプロセス起動/停止処理を既に持っており**（`architecture-manager.md`参照）、起動する主体が無かったわけではない——問題は、その起動処理を呼び出す条件（`running`チェック）を満たす手段が、`running`キャッシュがオンメモリでManager再起動のたびに無条件`unknown`へリセットされる設計のせいで、初回・Manager再起動直後には無かったこと
  - 初期案では、ワールドに触れずプロセスだけ起動/停止する専用コマンド`/activate`・`/deactivate`を新設する4コマンド構成を検討した。しかしレビューで「`/start`と`/activate`の役割分担（ワールドを作り直すか否か）がコマンド名から読み取れない」という指摘を受け、**`/start`自体の既定の挙動を「ワールドには触れずプロセスを起動するだけ」に変更し、ワールドの再生成は`/start clean`という修飾子で明示する3コマンド構成**（`/start [clean]`・`/load [force]`・`/deactivate`）に整理し直した。`/activate`はコマンドとして独立させず、新しい`/start`（`clean`無し）がその役割をそのまま引き継いだ
  - この再整理には副次的な利点があった：**`/start`（`clean`無し）の受理条件が「プロセスが既に起動中か」（Managerが`os/exec`で直接把握でき、常に正確）だけになり、`unknown`になりうる`running`値を一切参照しなくなった。** これにより`/start`のデッドロックは構造的に発生しなくなる——`running`の永続化（次項）とは独立した、より根本的な解決になっている
  - `running`値をオンメモリのキャッシュではなく**Manager自身のローカルディスクへ永続化**する方式に変更し、Manager自身の再起動をまたいで保持されるようにした。`unknown`（安全側で`true`扱い）は「プロセスは生きているがhardcoreとのTCP接続だけが切れている」場合にのみ限定し、「永続化された値が一度も存在しない」場合は`unknown`ではなく明確に`存在しない`（`running=false`と同じ扱い）とした。この永続化は`/start`のデッドロック解消には不要になったが、引き続き`/load`の`running`チェックの正確性（Manager再起動をまたいでも「挑戦が進行中です」を正しく判定できること）のために必要（2.1節「プロセス状態と`running`の永続化」）
  - 挑戦の状態（`running`：存在しない／進行中／終了）とプロセスの状態（起動中／停止中）を独立した2軸として整理し、両者の組み合わせ6通り（構造的に発生しない1通りを除く）それぞれについて`/start`・`/start clean`・`/load`・`/deactivate`の挙動を確定した（2.1節）

## 10. 未決事項

- Gate⇔Manager間シグナルの接続タイムアウト・リトライ回数（7.1節）
- MOD⇔Manager間シグナルの接続リトライ回数・バックオフ設定値（6.1節）
- Gate⇔Manager間の待受アドレス・ポート公開範囲の最終決定（Docker network限定で十分か、追加の認証・TLSを入れるか。7.1節）
- Manager障害時（プロセスクラッシュ・再起動）にGateがどう振る舞うか：接続断時は`state-query`を送らず「不明」として安全側に倒すことまでは決めたが（2.1節・3.1節）、再接続後の再同期手順の詳細は未確定
- PCF（Proxy-Compatible-Forge）の具体的なバージョン・設定詳細
- 権限ノード名の最終決定（`checkpoint.reset`等は旧Paper版からの仮称）
- lobby/hardcore間でのコード共有（commonモジュール化）の要否
- ボスMobの具体的なリストと、チェックポイント系/挑戦終了系それぞれへの分類（黄昏の森のどのボスをどちらにするか）
- `hardcoretogether`のテンプレート由来の`ModBlocks.kt`（`example_block`というカスタムBlockを登録）は「サーバーサイドのみで完結する」制約に反するため、実装時に削除する必要がある
- **既存の抜け**：`archive-request`が名前重複でManagerに拒否された場合、それを明示的にMODへ伝えるシグナルが6節に存在しない（MODは`archive-complete`が来ないことによる60秒タイムアウトでしか失敗を検知できない）。OPへのエラー表示がこの60秒待ちに引きずられる形になる。将来的に`archive-rejected`のような即時拒否シグナルを追加して解消するのが望ましいが、今回は既存の挙動を変えない範囲に留めた
- `/deactivate`が「プロセス正常停止」を待つ際のタイムアウト・強制kill方針（`server.properties`の`save-all`〜`stop`が異常に長引いた場合、Managerがどこまで待つか）。3.1節の「停止処理中」状態の抜け時間の上限は未確定

## 11. ディレクトリ構成

これまで各節に分散して書いていたパス情報をここに集約する。1節の通り、Manager・hardcoreサーバーは同一コンテナ（同一ホスト）上で動作する。**Gateはこれらのファイルシステムに一切アクセスしない**（ネットワーク経由でManagerへ問い合わせる、1節・7節参照）。

```
<Managerの作業ディレクトリ>/                  … Managerコンテナのルート
├── config.yml                               … Manager設定（archiveDir、hardcoreの起動コマンド・作業ディレクトリ、
│                                                signalPort、Gate向け待受アドレス等）
├── <archiveDir>/                             … 例: archive/（3.2節）
│   └── <name>/
│       ├── world/                           … アーカイブ時点のセーブフォルダの複製（SavedData含む）
│       └── meta.json                        … elapsedTime・createdAt（3.2節）
└── <hardcoreサーバーの作業ディレクトリ>/        … Managerがos/execで起動する子プロセスのルート
    ├── world/                               … セーブフォルダ。/startで削除・再生成される対象
    │   ├── data/                            … SavedData（running・challengeId。5.1節）
    │   └── ...                              … region, playerdata等、標準NeoForgeサーバー構成
    ├── records/                             … MOD管理。/startでも削除されない（5.5節）。パスはMODの
    │   │                                       storage.recordsDir設定で変更可（デフォルトがこの位置）
    │   └── <challengeId>.json               … 挑戦ごとのイベントログ
    ├── mods/, config/, server.properties 等 … 標準NeoForgeサーバー構成（本仕様の対象外）

<Gateの作業ディレクトリ>/
└── config.yml                                … Gate設定（lobby/hardcoreサーバーのアドレス、Manager接続先
                                                  アドレス等）。archiveDir・recordsDirへの参照は持たない
                                                  （Gateはファイルシステムにアクセスしないため）

<lobbyサーバーの作業ディレクトリ>/
└── world/playerdata/                        … チェックポイント座標はAttachmentType経由でプレイヤーデータに
                                                 埋め込まれる（4.1節）。専用ファイルは持たない
```

**役割の対比**

| パス | 管理主体 | 内容 | `/start`での扱い |
|---|---|---|---|
| `<Manager>/<hardcore作業ディレクトリ>/world/` | hardcoreサーバー本体（Managerの子プロセス） | ワールドの実データ＋`SavedData` | 削除・再生成される |
| `<Manager>/<hardcore作業ディレクトリ>/records/`（`storage.recordsDir`で変更可） | hardcore MOD（書き込み）／Manager（`/savedata`・`/senpan`用の読み取りのみ、3.3節） | 挑戦ごとのイベントログ（JSON） | 削除されない |
| `<Manager>/<archiveDir>/<name>/` | Manager | アーカイブされたワールドの複製＋メタデータ | 対象外（Manager管理のため） |
| `<lobby>/world/playerdata/` | lobby MOD（バニラ機構経由） | チェックポイント座標 | 対象外（別サーバー） |

`archive/<name>/meta.json`というファイル名は本節で初めて具体化した（3.2節では「メタデータをJSONとして保存する」とだけ書いていた）。実装時に別名にする場合はこの節を更新すること。
