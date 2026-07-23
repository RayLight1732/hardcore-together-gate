# Hardcore Together Manager アーキテクチャ設計

`specification.md`（以下「仕様書」）の内容を、このリポジトリ（**Managerのみ**）にどう実装として落とし込むかの設計。仕様書はGate/Manager2プロセス構成（仕様書1節）を前提としているが、**Gate・hardcore MOD・lobby MODはいずれも別プロジェクト（別リポジトリ）として実装される**ため、本ドキュメントの対象はManager側の実装に限定する。

**実装済み**：`go build ./...`・`go vet ./...`・`go test ./... -race`がすべて通り、実バイナリを起動してGate役・MOD役のTCPクライアントで`/start`・`/load`・アーカイブ・SIGTERM終了までエンドツーエンドに動作確認済み。当初はGo標準の機能パッケージ分割（`state`/`process`/`archive`/`records`/`modserver`/`gateserver`/`orchestrator`）で実装したが、その後**レイヤードアーキテクチャ（domain/port/application/adapter）へ再構成した**。兄弟リポジトリ`hardcore-together-neoforge`が`domain`・`port.ChallengeState`・`ChallengeApplicationService`・`adapter/neoforge`というポート&アダプタ（ヘキサゴナル）構成を採っているため、用語・構成をそちらに揃えている（1節参照）。

**実装済み（デッドロック修正・`/deactivate`）**：変更履歴の「初回`/start`が永遠に成功しないデッドロックを修正」（`running`の永続化＝`adapter/memstate`→`adapter/fsstate`、`/start`の`clean`修飾子・`/deactivate`＝8a節）・「孤児プロセス対策としてPIDファイルによる生存確認を追加」（3節）は実装済み。`go test ./... -race`・実バイナリE2E（13節の回帰テスト含む）で確認済み。実装中にdocker compose上での手動確認で見つかった追加のバグ（`/start`（`clean`無し）が`world/`未生成を誤って拒否していた）も修正済み——詳細は変更履歴末尾を参照。

Gate⇔Manager間・MOD⇔Manager間プロトコルの詳細（メッセージのフィールド定義・JSON例・シーケンス図）は`docs/protocol-gate-manager.md`・`docs/protocol-mod-manager.md`が正であり、本ドキュメントでは重複させず参照するに留める。

## 0. 前提・スコープ

- Managerは**常駐プロセス**であり、hardcoreサーバーを`os/exec`の子プロセスとして起動/停止/再起動する（仕様書1節）。この親子プロセス関係のため、Manager・hardcoreサーバーは同一コンテナ上で動作する必要がある
- Managerは2本のTCP+NDJSONサーバーを持つ：hardcore MOD向け（`docs/protocol-mod-manager.md`、`127.0.0.1`限定）とGate向け（`docs/protocol-gate-manager.md`、Docker network内）。**いずれもManagerがサーバー側（listenする側）**であり、MOD・Gateがそれぞれクライアントとして接続しにくる（両プロトコルの1節参照）
- hardcore MOD・lobby MOD・Gateの実装はブラックボックスとして扱う。Managerが提供する契約は上記2つのプロトコルドキュメントのみ

## 1. リポジトリ構成

**レイヤードアーキテクチャ（ports-and-adapters）を採用**。`domain`は純粋なルール・値のみ（I/O無し）、`port`はapplicationが依存するインターフェース、`application`はユースケース単位のサービス、`adapter`はport実装（ファイル・プロセス・TCP等の具体的なI/O）。依存の向きは常に「adapter → application → port ← domain」（applicationとdomainはどのadapterも知らない）。

```
cmd/manager/
  main.go                                   composition root。全adapterを構築し、port経由でapplicationへ注入し、
                                             graceful shutdown（SIGTERM等）まで面倒を見る（9節・8節）

cmd/fakehardcore/
  main.go                                   internal/e2eのテスト専用スタブ。製品には含めない。MOD⇔Manager
                                             プロトコルを最小限しゃべり、実Minecraftサーバー無しでE2Eを回す
                                             ためのヘルパーバイナリ（13節）

internal/
  domain/                                   純粋なルール・値オブジェクトのみ。I/O・時刻取得なし
    challenge/
      challenge.go                            Phase, Running, State型、DecideStart（force込みの
                                               許可判定の純粋関数、2節）
    archive/
      archive.go                              ResolveName（手動→拒否／自動→連番、existsは呼び出し側が注入）、
                                               DecideBaseName（4節）
    records/
      records.go                              Event, EventType, ChallengeRecord, PlayerRef, Trigger
                                               + AggregateSaveData / AggregateSenpan（純粋な集計、5節）

  port/
    port.go                                  ChallengeStateRepository, ProcessRunner, WorldPreparer,
                                              ArchiveRepository, RecordsRepository, GateNotifier,
                                              ReadyWaiter, Clock（各インターフェースの定義のみ）

  application/
    service.go                               ChallengeApplicationService：Start/Load/HandleArchiveRequest/
                                              HandleReady/HandleRunningChanged/HandleDisconnect/
                                              Snapshot/SaveData/Senpan。opMutexもここに内包（8節）

  adapter/
    fsstate/        fsstate.go               port.ChallengeStateRepositoryの実装。`{phase, running}`はオンメモリで
                                              保持しつつ、`running`の変化はローカルディスクの小さな状態ファイル
                                              へも同期的に書き込み、Manager自身の再起動をまたいで復元する（2節。
                                              旧`adapter/memstate`から改名）
    osprocess/      process.go, worldgen.go  port.ProcessRunner・WorldPreparerのos/exec実装（3節）
    fsarchive/      fsarchive.go, restore.go port.ArchiveRepositoryのファイルシステム実装。
                                              domain/archiveの命名ルールを内部で使用（4節）
    fsrecords/      fsrecords.go             port.RecordsRepositoryの読み取り専用ファイル実装（5節）
    systemclock/    systemclock.go           port.Clockのtime.Nowラッパー
    modserver/      server.go, handler.go    MOD⇔Manager TCP+NDJSONサーバー。port.ReadyWaiterを実装しつつ、
                                              業務判断は全てApplicationインターフェース経由で委譲する薄い
                                              アダプタ（6節、docs/protocol-mod-manager.md準拠）
    gateserver/     server.go, handler.go    Gate⇔Manager TCP+NDJSONサーバー。port.GateNotifierを実装しつつ、
                                              同様にApplicationへ委譲する薄いアダプタ（7節、
                                              docs/protocol-gate-manager.md準拠）
    config/         config.go                config.yml読み込み・バリデーション（9節）

  ndjson/
    ndjson.go                                MOD⇔Manager・Gate⇔Manager共通のNDJSON読み書きヘルパー。
                                              業務的な層分けの外側にある共有ユーティリティ（意図的に
                                              domain/port/application/adapterのどれにも属さない）

  e2e/
    e2e_test.go                              cmd/manager・cmd/fakehardcoreを実際にビルド・起動する唯一の
                                              真のE2Eテスト（13節）
```

**modserver・gateserverとapplicationの相互参照**：`ChallengeApplicationService`は`port.ReadyWaiter`（modserverが実装）・`port.GateNotifier`（gateserverが実装）に依存する一方、modserver/gateserver自身も受信メッセージの処理を`Application`インターフェース（各adapterパッケージ内で個別に定義する小さなインターフェース）経由でサービスへ委譲する。この相互依存を素朴に構築しようとすると循環するため、`modserver.NewServer`・`gateserver.NewServer`は`Application`を受け取らずに構築し、`ChallengeApplicationService`を構築した後で`SetApplication`により後から注入する二段階構築にしている（8節・cmd/manager/main.go参照）。

Gate側リポジトリの`architecture-gate.md`（別リポジトリ）冒頭に「Manager側の実装設計はManager側プロジェクトの`architecture.md`（別リポジトリ）に記載される想定」という記述があるが、本ドキュメント（`architecture-manager.md`）がそれに相当する。

## 2. 状態管理設計（`domain/challenge` + `port.ChallengeStateRepository` + `adapter/fsstate`）

Managerが内部で持つ状態は2つで、常にペアで扱う（仕様書3.1節）。

| 状態 | 型 | 意味 |
|---|---|---|
| `Phase` | `stopped` \| `starting` \| `ready` \| `stopping` | プロセスライフサイクル上の4値状態（仕様書3.1節の状態遷移図。`stopping`は`/deactivate`受理〜プロセス終了確認までの間、後述8a節） |
| `Running` | `true` \| `false` \| `unknown` | 挑戦が進行中かどうかの値。hardcore MODからの`ready`/`running-changed`で更新され、**Manager自身のローカルディスクへ永続化される**（後述） |

**レイヤーごとの役割分担**：
- `domain/challenge`：`Phase`/`Running`/`Snapshot`型と、`DecideStart(phase Phase, current Running, force bool) (ok bool, reason string)`という**純粋な**許可判定ルールのみを持つ。`phase`が`starting`/`stopping`（遷移中）の場合は`force`の値に関わらず「処理中です」で拒否するガードを`running`チェックより先に持つ（仕様書2.1節「起動処理中・停止処理中」の一律拒否ルール。`clean:true`もこのガードの対象——後述）。同様に`DecideResume(phase Phase)`（`start{clean:false}`用）・`DecideDeactivate(phase Phase)`（`deactivate`用）も持つ。I/Oも排他制御も知らない
- `port.ChallengeStateRepository`：`Snapshot()`・`TryMarkStarting(force bool) (bool, string)`・`TryMarkResuming() (bool, string)`・`TryMarkDeactivating() (bool, string)`・`MarkReady`・`SetRunning`・`MarkUnknown`・`MarkStopped`・`MarkDeactivated`・`Restore(State)`というインターフェース定義のみ。`TryMarkResuming`は`start{clean:false}`専用の判定（`running`を一切見ず、`phase==stopped`かどうかだけを見る。8a節）。`MarkDeactivated`は`MarkStopped`と異なり`phase`のみを`stopped`に戻し`running`は変更しない（`deactivate`の成功パス・`start{clean:false}`のプロセス起動失敗パス専用、8a節）
- `adapter/fsstate.Repository`（**旧`adapter/memstate`から改名**、後述「`running`の永続化」）：上記portの実装。1つの`sync.RWMutex`で`{phase, running}`のタプルを保護し、`TryMarkStarting`/`TryMarkResuming`/`TryMarkDeactivating`内部で判定ロジックを呼びつつ、許可された場合の状態遷移コミットまでを**同一ロック内でアトミックに**行う（フィールドを直接触らせない）

`DecideStart`（判定ルール）と`TryMarkStarting`（判定＋コミットのアトミックな実行）をあえて分けている理由：判定ロジック自体は`adapter/fsstate`のテストとは独立に`domain/challenge`単体でユニットテストでき（ロック・並行性の心配が要らない）、一方で「判定してからコミットするまでの間に割り込まれない」という排他性の保証はport実装（adapter）側の責務として残せるため。

- `phase`が`starting`/`stopping`の間（起動処理中・停止処理中）は、`DecideStart`/`DecideResume`/`DecideDeactivate`いずれも`force`/`clean`の値に関わらず「処理中です」で一律拒否する（前述）。**これにより「処理中に別の`/start`・`/load`・`/deactivate`が来たらどうするか」を専用のロックで排他する必要が無い**——`phase`そのものを見て自動的に弾かれるため、`clean:true`・`force:true`のような`running`チェックを免除する指定でも素通りしない（12節）

### `running`の永続化とデッドロックの修正

**当初の設計にはバグがあった**：Manager起動直後・MOD⇔Manager接続切断中は無条件で`running=unknown`とし、Gate側の`running`チェック（`/start`・`/load`）は`unknown`を`true`と同じ扱い（拒否）にする、という「安全側デフォルト」だけを持っていた。これは実装され`go test ./... -race`・実バイナリE2Eまで確認済みだったが、一度もhardcoreを起動したことが無い状態（真新しいデプロイ）でも`unknown`＝拒否となり、`/start`を拒否から解放できるのはhardcore自身が起動して`ready`を送ってきた場合のみ、その`ready`を得るための`/start`自体が拒否され続ける、というデッドロックが存在した（`specification.md` 2.1節「背景」参照）。

**修正**：`running`をオンメモリの一時値ではなく、**`adapter/fsstate`がローカルディスクへ永続化する値**に変更した。

- `SetRunning(running bool)`・`MarkReady(running bool)`が呼ばれるたび、`{"running": <bool>}`のような小さなJSONファイル（パスは`config.yml`で指定、9節）へ同期的に書き込む。`MarkUnknown()`はディスクへは書かない（`unknown`は「プロセスは生きているがMOD接続だけが切れている」という、Manager自身の現在の生存プロセスに紐づく一時的な状態であり、Manager再起動後は`phase`がどのみち`stopped`にリセットされる以上、意味を持たないため）
- Manager起動時、`adapter/fsstate.New(...)`はこの永続化ファイルを読み込む：**存在すれば**その`running`値を初期値として使う（Manager自身が再起動しても、直前の挑戦状態を失わない）。**存在しなければ**（一度も`/start`・`/load`が成功したことが無い、真新しいデプロイ）`running=false`で初期化する——`unknown`ではない。`phase`は常に`stopped`で初期化する（Manager再起動時、`os/exec`の子プロセスは道連れで死んだものとして扱い、生存中の子プロセスへの再アタッチはサポートしない。旧14節未確定事項6の結論、後述）。**ただしこの「道連れで死ぬ」という前提は、Managerへ`SIGTERM`が届く通常の再起動（Docker再起動等）でしか保証されない**——Manager自体がpanic・OOM Kill・`SIGKILL`で即死した場合はグレースフルシャットダウン処理が一切走らず、hardcoreプロセスが孤児として生き残ったまま新しいManagerが`phase=stopped`で起動しうる。この状態を放置すると、次の`/start`（`clean`無し）が「動いていない」と誤認して**同じ`world/`・同じポートへ向けて二重にプロセスを起動する**危険がある。この危険を防ぐため、Manager起動時に**PIDファイルによる生存確認**を行う（詳細は3節）
- 「永続化ファイルが存在しない」＝「一度も`/start`・`/load`が成功していない」であり、`world/`ディレクトリの存在ともほぼ同義になる（両者は`Start`/`Load`の成功時に同時に作られるため）。ただし`start{clean:false}`は`world/`の有無を一切判定に使わない（後述：無ければ単にプロセス起動時に新規作成されるだけで、拒否はしない、仕様書2.1節）ため、この永続化はもっぱら`/load`の`running`チェックの正確性のために必要になる（`TryMarkResuming`は`phase`のみを見るため`running`永続化に依存しない）

この変更により、`unknown`（安全側で`true`扱い）は「プロセスは生きているのにMOD接続だけが切れている」というまれな異常系にのみ発生するようになり、一度も起動したことが無い状態からの`/start`が阻まれることは無くなった。

## 3. プロセスライフサイクル管理（`port.ProcessRunner`・`port.WorldPreparer` + `adapter/osprocess`）

`os/exec.Cmd`でhardcoreサーバーを子プロセスとして起動する。この節の内容は全て`adapter/osprocess.Runner`（`port.ProcessRunner`・`port.WorldPreparer`両方を実装）に集約されており、純粋なドメインルールが無いため`domain/`配下には対応パッケージが無い（起動/停止・ファイル操作それ自体がI/Oであり、抽出できる「判定ロジック」が無いため）。

- **起動**：`exec.CommandContext`＋`config.hardcore.startCommand`（9節）。標準出力/標準エラーはManagerのログへ行単位で転送する（クラッシュ時の調査用）。`cmd.Wait()`の結果（exit code）はログに残すのみで、Manager側のビジネスロジックはMOD⇔Manager接続の断（2節）で検知する
- **停止**：まずプロセスへ`SIGTERM`（NeoForge/バニラサーバーは`stop`コマンド相当のgraceful shutdownに対応しないため、シグナルベースで統一する）を送り、`cmd.Wait()`をタイムアウト付きで待つ。タイムアウトした場合のみ`SIGKILL`にエスカレーションする
- **ワールドの新規生成（`/start`用）**：`world/`ディレクトリを削除するだけで、テンプレートのコピーは行わない。**シード値は都度やり直したい**（毎回ランダムな新しいワールドで挑戦する）ため、あらかじめ焼き込んだワールドを複製する方式は採らず、`world/`が存在しない状態でプロセスを起動し、NeoForge（バニラ準拠）自身に新規ワールドを生成させる
- **hardcoreモード・難易度HARDの固定方法**：バニラサーバーは`server.properties`の`hardcore=true`を新規ワールド生成時に読むと、そのワールドをハードコアモード（難易度HARD固定・死亡でスペクテイター送り）で生成する、という標準機能を持つ（NeoForgeもこれをそのまま継承しており、MOD側でランタイムに`setHardcore`を呼ぶ必要はない）。同様に`level-seed`を空にしておけば、新規生成のたびにランダムなシードが使われる。つまり**「テンプレートに焼き込む」必要は無く、`hardcore/`作業ディレクトリに置く`server.properties`で`hardcore=true`・`level-seed=`（空）にしておくだけで、仕様書5.3節の要件（ランタイムでの`setHardcore`相当APIが無い制約下でのhardcore固定）とユーザーが望む「シードは都度やり直す」の両方を満たせる
- **Managerによる`server.properties`の保証**：`server.properties`自体は`world/`の外にあり`/start`のワイプ対象ではない（仕様書11節）ため、通常は初期セットアップ時に設定した値がそのまま残り続ける想定だが、手動編集等で`hardcore=true`が意図せず外れる事故を防ぐため、Managerは`/start`時に`world/`を削除する前後で`server.properties`の`hardcore=true`を読み取り検証し、`false`になっていた場合は書き戻す（`level-seed`は明示的に空へ強制はしない——運用上あえて固定シードでテストしたいケースを妨げないため。デフォルトで空にしておく運用は初期セットアップ側の責務とする）
- **`records/`はワイプ対象に含めない**：`world/`と同階層だが別ディレクトリなので、`world/`削除処理は`records/`に触れない（仕様書11節の table通り）
- **`Exists() (bool, error)`**：`world/`ディレクトリの存在を確認するだけの薄いメソッド。`port.WorldPreparer`に定義されている。**当初`start{clean:false}`が「ワールドが存在しません」で拒否するために使う設計だったが、これは仕様書2.1節の状態別挙動表（状態①：`world/`が無ければ拒否ではなく新規作成して起動）と矛盾する誤りだったため、`application`層からの呼び出しは削除した**（変更履歴末尾参照）。`world/`が無い場合の新規作成は、Manager側が事前にチェックするのではなく、`process.Start()`でhardcore自身を起動した際にNeoForge側が自然に行う（3節「ワールドの新規生成」）

### PIDファイルによる孤児プロセス検知（`adapter/osprocess`）

2節で述べた通り、「Manager再起動時は子プロセスも道連れで死んでいる」という前提は`SIGTERM`によるグレースフルシャットダウンでしか保証されず、Manager自体がクラッシュ（panic・OOM Kill・`SIGKILL`）した場合は保証されない。これを検知・解消するため、**Manager起動時にPIDファイルで既存プロセスの生存確認を行う**。

- **書き込み**：`process.Start()`が子プロセスの起動に成功した直後、そのPIDを`config.hardcore.pidFile`（例：`./hardcore.pid`、Managerの作業ディレクトリ直下。`hardcore/`作業ディレクトリの外に置く——NeoForgeが`hardcore/`配下を独自に管理するため、Manager自身の管理ファイルは混在させない）へ書き込む
- **削除**：`process.Stop()`が正常終了（`cmd.Wait()`の完了）を確認した直後、PIDファイルを削除する。`SIGKILL`エスカレーションで強制終了させた場合も、終了を確認できた時点で同様に削除する
- **起動時の生存確認**：Manager起動時（`cmd/manager/main.go`の初期化シーケンス内、Gate⇔Manager用TCPサーバーが`start`等のコマンドを受け付け始める**前**）、`adapter/osprocess.Runner`の初期化時に以下を行う：
  1. PIDファイルが存在しない → 何もしない（通常の`phase=stopped`起動）
  2. PIDファイルが存在する → 記載されたPIDのプロセスが生きているかを確認する（Unix系では`kill(pid, 0)`相当のシグナル0送信、またはそれが失敗した場合は`/proc/<pid>/`の存在確認）。**PIDの再利用（OS再起動等で無関係な別プロセスに同じPIDが割り当てられている）による誤検知を減らすため**、可能であれば`/proc/<pid>/cmdline`等でプロセスの実行コマンドが`config.hardcore.startCommand`と一致するかも合わせて確認する
     - 生きていない（または実行コマンドが一致しない）→ 古いPIDファイルとみなして削除し、通常通り`phase=stopped`で起動を続行する
     - **生きている（＝孤児プロセスを検知）** → ログに警告を出力した上で、そのプロセスへ`SIGTERM`→タイムアウト後`SIGKILL`（3節の`process.Stop()`と同じエスカレーション）を送って確実に終了させ、PIDファイルを削除してから`phase=stopped`での起動を続行する。**この確認・終了処理が完了するまで、Gate⇔Manager用TCPサーバーはリッスンを開始しない**（`start`等のコマンドが孤児プロセスの存在に気づかず処理されてしまう窓を作らないため）
- この仕組みにより、2節で述べた「二重起動」のリスクは、Manager起動シーケンスの最初期に確実に解消されてから通常運用に入る

（このPIDファイルは`state.json`〈2節〉とは別ファイルであり、役割も異なる——`state.json`は「挑戦が進行中かどうか」という論理的な状態、PIDファイルは「実際に子プロセスが動いているか」という物理的な事実の記録）

## 4. アーカイブ実行（`domain/archive` + `port.ArchiveRepository` + `adapter/fsarchive`）

**レイヤーごとの役割分担**：
- `domain/archive`：`DecideBaseName(name string, now time.Time) string`（手動なら`name`そのまま、自動なら`now`を整形）と`ResolveName(base string, manual bool, exists func(string)(bool,error)) (string, error)`（重複時の拒否/連番付与ロジック）という**純粋関数**のみ。`exists`チェック自体はI/Oなので呼び出し側（adapter）が注入する
- `port.ArchiveRepository`：`Exists`・`Latest`・`Restore`・`Save(name, elapsedTime, now) (finalName string, err error)`のインターフェース定義のみ
- `adapter/fsarchive.Repository`：上記portの実装。`Save`内部で`domain/archive`の2関数を使いつつ、実際のファイルコピー・`meta.json`書き込みを行う

`Save`（旧`archive-request`ハンドラのExecute相当）の処理内容（`HandleArchiveRequest`経由、6節・application層8節。仕様書3.2節）：

1. `now`（呼び出し元のapplication層が`port.Clock`から取得し、`Save`へ渡す。このタイムスタンプを`createdAt`、および`name`省略時は`name`の生成にも使う——両者が同一の値になるよう、必ず1回だけ読んで使い回す）
2. `domain/archive.DecideBaseName`で基準名を決定：受信した`name`が空でなければそのまま、空（省略）なら`now`を`2026-07-18T12-34-56`形式に整形
3. `domain/archive.ResolveName`で最終名を決定：`archive/<name>/`が存在すれば、手動なら拒否（`archive-complete`を返さない。7節参照の`archive-rejected`案は未実装）／自動なら末尾へ連番を付与
4. `world/` → `archive/<name>/world/`をコピー（hardcoreプロセスは止めない。MOD側が`save-off`→`save-all flush`済みの状態で送ってくる前提、5.2〜5.3節）
5. `archive/<name>/meta.json`に`{"elapsedTime": ..., "createdAt": now}`を書き込む（仕様書11節でファイル名を確定済み）
6. 実際に採用した`name`を返す（`adapter/modserver`が`archive-complete{name: ...}`としてMODへ送信）

**`name`・`createdAt`の生成元をManagerに一本化した**（`docs/protocol-mod-manager.md` 3.3節）。`name`を送った場合は拒否、省略した場合は連番付与、という分岐自体は変わらないが、この分岐は「手動/自動」を区別する専用フィールドではなく**`name`が空かどうかだけ**で判定する。理由：
- 当初、Manager側だけでは手動/自動の区別がつかない抜けがあった。`name`の命名規則〔タイムスタンプ形式か否か〕から推測する案も検討したが結合度が高く見送り、`origin`（`"manual"` | `"auto"`）フィールドを追加して解消した
- その後さらに見直し、`origin`自体を廃止した。`name`・`createdAt`の生成元をMODからManagerへ移した結果、`name`は「送るか省略するか」の任意フィールドになり、この有無自体が手動/自動を過不足なく表すようになったため、重ねて`origin`を持つのは冗長だった
- あわせて、ボス討伐時の日時整形・名前生成ロジックをMOD側に持たせる必然性も無い（実際にファイルコピーとタイムスタンプ発行を行うのはManagerであり、MOD・Managerは同一コンテナ上でクロックを共有しているため、MODが別途計測・整形して送る意味が無い）と判断し、`name`（省略時）・`createdAt`（常に）ともにManager側で生成する設計にした
- この変更により、`name`を省略した場合MODは`archive-request`送信時点で最終的な`name`を知らない。`archive-complete`の`name`で通知し、MODはそれを5.5節のイベントログ（`archiveName`）等に使う

- **`/load`用の復元（`adapter/fsarchive/restore.go`の`Restore`）**：`archive/<name>/world/` → `world/`のコピー。`Restore`自体はコピーのみを行い、`world/`の削除は呼び出し側（`application.ChallengeApplicationService`）の責務とする（`os.CopyFS`は既存ファイルを上書きしないため、コピー前に必ず`world/`を空にしておく必要がある）。**実装時の教訓**：レイヤー分割前の実装で、この削除呼び出しを`Load`の準備処理に配線し忘れ、`/load`実行時に`file exists`エラーで失敗するバグが実際に発生した（`cmd/manager`でManagerを実際に起動し、`/start`→アーカイブ→`/load latest`という一連の操作をエンドツーエンドで試して発見。ユニットテストだけでは`Load`のprepare関数内をモック済みの`ArchiveRepository.Restore`が素通りしてしまい検出できなかった）。8節の疑似コードに`WorldPreparer.WipeWorld`の呼び出しを明記して修正し、レイヤー分割後も維持している
- **排他制御**：「アーカイブ実行中は`/start`・`/load`をブロックする」（仕様書3.2節）を、`application.ChallengeApplicationService`が内部に持つ1本の`sync.Mutex`（`opMutex`）で実現する。`HandleArchiveRequest`（アーカイブコピー）も、`Start`/`Load`のプロセス再起動シーケンス（8節）も、この同じ`opMutex`を獲得してから実行する。仕様書の文言が「ブロックする」（＝拒否ではなく待たせる）である以上、`TryLock`ではなく`Lock()`（ブロッキング）を使う——アーカイブコピーは通常数秒〜数十秒で終わる短時間処理なので、`/start`・`/load`側が多少待たされても実用上問題ない。**レイヤー分割前は`modserver`にも`opMutex`を共有する必要があったが**（`archive-request`受信時に`modserver`自身がアーカイブ処理を呼んでいたため）、`HandleArchiveRequest`をapplication層に集約した結果、`opMutex`は完全にapplication層内部に閉じ込められるようになった（cmd/manager側で`*sync.Mutex`を作って複数箇所へ配る必要が無くなった）

## 5. 挑戦記録の読み取り（`domain/records` + `port.RecordsRepository` + `adapter/fsrecords`）

**レイヤーごとの役割分担**：
- `domain/records`：`Event`/`EventType`/`ChallengeRecord`/`PlayerRef`/`Trigger`型と、`AggregateSaveData([]ChallengeRecord) []SaveDataEntry`・`AggregateSenpan([]ChallengeRecord) []SenpanEntry`という**純粋な集計関数**のみ。ファイルを1つも開かないため、一時ファイルを作らずGoの構造体リテラルだけでユニットテストできる
- `port.RecordsRepository`：`ReadAll() ([]ChallengeRecord, error)`のインターフェース定義のみ（生データの読み取りのみ、集計は含まない）
- `adapter/fsrecords.Repository`：上記portの実装。`config.hardcore.recordsDir`配下の`*.json`を全件走査し、各ファイルを`ChallengeRecord`としてパースするだけ（仕様書5.5節のファイル構造）

- **書き込みは行わない**（書き込みはhardcore MODの責務、仕様書3.3節）。ファイルロック等の配慮も不要（Managerは読み取り専用）
- `/savedata`：`application.ChallengeApplicationService.SaveData()`が`ReadAll()`＋`AggregateSaveData`を呼び、全ファイルの`events`を`challengeId`付きでフラットにマージして返す（`savedata-response`、`docs/protocol-gate-manager.md` 3.6節）
- `/senpan list|count`：同様に`Senpan()`が`ReadAll()`＋`AggregateSenpan`を呼び、`type: death`のみを抽出し`deadPlayer.uuid`でグルーピングして件数・一覧を返す（`senpan-response`、同3.7節）
- **`config.hardcore.recordsDir`はhardcore MODの`storage.recordsDir`と値を一致させる必要がある**（仕様書3.3節・5.5節）。Managerはこの一致を実行時に検証できない（MOD側の設定ファイルを直接読まないため）ので、`config.yml`のコメントで明記するに留める

## 6. MOD⇔Manager サーバー（`adapter/modserver`、`port.ReadyWaiter`実装）

`docs/protocol-mod-manager.md`のサーバー側実装。`127.0.0.1:<signalPort>`でリッスンし、hardcore MODからの接続を受け付ける（1節）。**業務判断は一切持たない薄いアダプタ**であり、NDJSONの解析・組み立てのみを行い、実際の処理はすべて`Application`インターフェース（`application.ChallengeApplicationService`が満たす、`modserver`パッケージ内で定義する小さなインターフェース）へ委譲する。

- MODは`ServerStartedEvent`発火時に接続しにくるクライアント側であり、Managerは常時リッスンしているだけでよい。1本のTCP接続を「現在のhardcoreプロセスとの接続」として保持する（同時に複数のhardcoreプロセスが動くことは無い前提、1節）
- 受信：`ready`（`Application.HandleReady(running)`を呼ぶ）、`running-changed`（`Application.HandleRunningChanged(running)`）、`archive-request`（`Application.HandleArchiveRequest(name, elapsedTime)`を呼び、完了後`archive-complete`を送信）
- 接続が切れたら`Application.HandleDisconnect()`を呼ぶ（2節の安全側デフォルト、仕様書6.1節の「接続断の扱い」）
- 接続断後の新規接続を新しい「現在の接続」として扱う（`/start`・`/load`で子プロセスが再起動されるたびにMOD側は再接続してくるため）
- **`port.ReadyWaiter`の実装元でもある**：`WaitForReady`/`DrainReady`はこのパッケージ自身が保持する`readyCh`チャネルで実現しており、`ready`受信時は「`Application.HandleReady`を呼ぶ」と「`readyCh`へ値を流し`WaitForReady`を解放する」の両方を行う。前者は状態遷移という業務ロジック、後者はこのTCPアダプタ固有の非同期待ち合わせ機構であり、両者は別の関心事として扱う

## 7. Gate⇔Manager サーバー（`adapter/gateserver`、`port.GateNotifier`実装）

`docs/protocol-gate-manager.md`のサーバー側実装。設定可能なアドレス（Docker network内限定、ホストへは公開しない）でリッスンする。6節同様、業務判断を持たない薄いアダプタで、`Application`インターフェース（`gateserver`パッケージ内で定義）へ委譲する。

| 受信 | 処理 |
|---|---|
| `state-query` | `Application.Snapshot()`をそのまま`state-response`として返す（同期応答） |
| `start` | `Application.Start(ctx, clean, requestedBy)`を呼ぶ（8節・8a節。`clean`の値で内部分岐する） |
| `load` | `Application.Load(ctx, name, force, requestedBy)`を呼ぶ（8節） |
| `deactivate` | `Application.Deactivate(ctx, requestedBy)`を呼ぶ（8a節） |
| `savedata-query` | `Application.SaveData()`の結果を`savedata-response`で返す |
| `senpan-query` | `Application.Senpan()`の結果を`senpan-response`で返す |

送信（`application.ChallengeApplicationService`からのコールバック経由、`port.GateNotifier`実装として）：`start-rejected`/`load-rejected`/`deactivate-rejected`（拒否理由付き）、`evacuate-request`→`evacuate-complete`待ち、`hardcore-ready`、`deactivate-complete`。

Gate側は起動時に接続しにくるクライアントであり、Managerは常時リッスンする。Gate接続が切れている間に`start`/`load`は届かないため、application層で「Gate接続の有無」を気にする必要は無い（Gateが状態不明として振る舞うだけ、仕様書2.1節）。

## 8. `/start`・`/load`シーケンスの実装（`application.ChallengeApplicationService`）

仕様書7.3節のフローをそのままコードへ落とし込む中心コンポーネント。レイヤードアーキテクチャ化に伴い、旧`orchestrator`パッケージの内容はここへ移った。`opMutex`もこの構造体に内包され（`sync.Mutex`、外部と共有する`*sync.Mutex`ではない）、`Start`・`Load`・`HandleArchiveRequest`（6節）が同じインスタンスの同じロックを使う。

**実装時に、当初の疑似コード（後述）にあった手順の順序を1点修正した**：opMutexをrunningチェックより先に獲得する案だと、先発の`/start`がシーケンス全体（退避待ち〜再起動〜ready待ち、最大で数十秒〜数分）の間opMutexを握り続けるため、後発の`/start`はrunningチェックにたどり着く前にopMutex獲得待ちで長時間ブロックされてしまう。これは2節の「`DecideStart`自身の遷移中ガードが2件目の`/start`を即座に弾く」という説明と矛盾する。そこで、`ChallengeStateRepository.TryMarkStarting`（2節、ロック不要のアトミックな検査兼コミット）を**先に**実行し、opMutexはその後、実際にファイル/プロセスへ触れる直前でのみ獲得する順序に改めた：

`Start`は`clean`フラグで2つの経路に分岐する。`clean==true`のときの経路は以下の通り（`clean==false`の経路は8a節）：

```
Start(ctx, clean bool, requestedBy string) error:
  clean==false の場合 → 8a節の経路へ（このStart pseudocodeには入らない）

  // clean==true
  1. prior := state.Snapshot()（失敗時のロールバック用に退避）
  2. state.TryMarkStarting(force=true)（`clean`はrunningチェックを常に免除するため、
     実質「force固定」と同じ意味になる。ただしphaseが遷移中〈starting/stopping〉なら
     forceでも拒否される〈domain/challenge.DecideStartの遷移中ガード、2節〉。
     ここはopMutex不要）
  3. opMutex.Lock() → defer Unlock()
  4. reason := "force-reset"（`docs/protocol-gate-manager.md` 3.5節：`start`のclean:trueは
     常にforce-reset固定。`prior.Running`の値では分岐しない——`load`のforce:trueとはここが異なる）
     port.GateNotifier経由で evacuate-request(reason) 送信（その時点でphase!=stoppedの場合のみ。
     stoppedなら手順4〜5は省略し手順5aへ）
     → evacuate-complete受信までブロック（タイムアウト付き、14節）
     失敗時: state.Restore(prior)（何も壊していないので手順1の状態へ戻す）
  5. process.Stop()（3節、port.ProcessRunner。手順4を経由した場合のみ）
     失敗時: state.MarkUnknown()（停止できたか不明。phaseはstartingのまま＝forceでしか抜けられない安全側）
  5a. world.WipeWorld()＋world.EnsureHardcoreMode()（3節、port.WorldPreparer。Loadの場合は
     archives.Restore、後述）
     失敗時: state.MarkStopped()（旧プロセスの停止は確認済み、新プロセスも無い＝running=false は正確）
  6. ready.DrainReady() → process.Start()（3節）
     process.Start失敗時: state.MarkStopped()
  7. port.ReadyWaiter からの ready 受信を待つ（タイムアウト付き）
     受信時 state.MarkReady(running) が呼ばれる（6節、modserverのHandleReady経由。ここでは何もしない）
     タイムアウト時: state.MarkUnknown()（起動できたか不明。遅れてreadyが届けばmodserver側が
     独立に補正するので、ここでは安全側に倒すだけでよい）
  8. port.GateNotifier経由で hardcore-ready 送信
```

`Load(ctx, name, force, requestedBy)`もほぼ同じ流れだが：
- 手順2の直後（opMutex獲得**前**）に追加のアーカイブ存在チェックを行う：`archive/<name>/`の有無、`name=="latest"`の場合は全`meta.json`の`createdAt`を比較して最新を選ぶ。存在しなければ`state.Restore(prior)`で手順2の状態へ戻し、`load-rejected`を送る。**このチェックの前にrunningチェック（手順2）を済ませておくことで、「runningがtrueかつ指定アーカイブも存在しない」場合に仕様書2.1節の想定通り「挑戦が進行中です」が優先される**（アーカイブ不在エラーではなく）
- 手順5aが「テンプレートコピー」の代わりに「`world.WipeWorld()` → `archives.Restore(name)`（4節）→ `world.EnsureHardcoreMode()`」になる。`archives.Restore`自体はコピーのみでworld/の削除は行わないため、`Start`と同じく明示的な`WipeWorld`呼び出しが必要（4節参照。ここを配線し忘れた実装バグが一度発生し、修正済み）。`EnsureHardcoreMode`はStartと同じ理由（`server.properties`はworld/の外にあり、アーカイブの復元対象に含まれないため）でLoadでも呼ぶ

- **タイムアウト**：手順4（`evacuate-complete`待ち）・手順7（`ready`待ち）はいずれも無期限ブロックしない。具体的な秒数は14節の未確定事項（Gate側の`architecture-gate.md`にも同種の未確定事項があり、双方で値を揃える必要がある）
- **`opMutex`は`Start`/`Load`/`Deactivate`/`HandleArchiveRequest`（8a節・6節・4節）で共有する**唯一のロックであり、「進行中は片方をブロックする」という仕様書3.2節の要求をこれ1本で満たす
- **失敗時のstate復旧はいずれも仕様書に明記が無く、実装時に補った**：どこまで進んだ時点で失敗したかによって「安全に主張できる内容」が異なる（手順4失敗＝何も壊していないので直前の状態に戻せる、手順5失敗＝停止できたか不明なので`unknown`、手順5a・6失敗＝旧プロセスの停止は確認済みなので`running=false`は正確、手順7失敗＝新プロセスが実際には生きているかもしれないので`unknown`）という考え方で使い分けている

## 8a. `Start`（`clean:false`）・`Deactivate`の実装（`application.ChallengeApplicationService`）

`specification.md` 2.1節・7.4節で定義した`/start`（`clean`無し）・`/deactivate`の実装。`Start`（`clean:true`、8節）・`Load`が内部で行っている「プロセス起動」手順の後半を、ワールド操作（新規作成・復元）を伴わない単独の経路として切り出したものであり、`adapter/osprocess.Runner`（3節）・`port.GateNotifier`・`port.ReadyWaiter`は共有する（新規のI/Oアダプタは追加しない。新規に必要なのは2節の`TryMarkResuming`/`TryMarkDeactivating`/`MarkDeactivated`のみ）。

`start{clean:false}`は`world/`の有無を一切判定に使わない点に注意（3節「`Exists()`」参照：当初はここで`world.Exists()`を呼び「ワールドが存在しません」で拒否する設計だったが、仕様書2.1節の状態別挙動表（状態①）と矛盾する誤りだったため削除した）。`world/`が無ければ、手順3の`process.Start()`でhardcoreプロセス自身が新規ワールドを生成するだけで、Manager側の拒否分岐は無い。

```
Start(ctx, clean=false, requestedBy string) error:
  1. ok, reason := state.TryMarkResuming()（2節。phase!=stoppedの場合のみ拒否——
     「既に起動しています」〈phase==ready〉・「処理中です」〈phase==starting/stopping〉のいずれか。
     **runningは一切参照しない**）
     ok==false なら start-rejected(reason) を送って終了
  2. opMutex.Lock() → defer Unlock()（Start(clean=true)/Loadと同じロックを使う。3節のプロセス操作
     を保護するという意味では一貫しているが、対象状態は常にphase==stoppedのはずなのでStart/Load
     のアーカイブコピーと衝突する実害は無い。念のため揃えているだけ）
  3. ready.DrainReady() → process.Start()（3節。ワールドには一切触れない。world/が無ければ
     hardcoreプロセス自身が新規生成する）
     process.Start失敗時: state.MarkDeactivated()（**`MarkStopped`ではない**——world/は一切
     触れていないので、進行中だった挑戦（running）を書き換えてはならない。phaseのみstoppedへ戻す）
  4. port.ReadyWaiterからのready受信を待つ（タイムアウト付き、14節）
     受信時 state.MarkReady(running) が呼ばれる（6節、modserverのHandleReady経由。
     runningは変更前の値がそのまま渡ってくる——プロセス起動だけでは挑戦の状態は変わらない）
     タイムアウト時: state.MarkUnknown()
  5. port.GateNotifier経由で hardcore-ready 送信

Deactivate(ctx, requestedBy string) error:
  1. ok, reason := state.TryMarkDeactivating()（2節。phase!=readyの場合は拒否——
     「既に停止しています」〈phase==stopped〉・「処理中です」〈phase==starting/stopping〉のいずれか）
     ok==false なら deactivate-rejected(reason) を送って終了
  2. opMutex.Lock() → defer Unlock()
  3. port.GateNotifier経由で evacuate-request(reason="deactivate") 送信
     → evacuate-complete受信までブロック（タイムアウト付き、14節）
  4. process.Stop()（3節、SIGTERM→タイムアウト後SIGKILL）
  5. state.MarkDeactivated()（**`running`値は変更しない**——中断していた挑戦の状態をそのまま保持する。
     Start(clean=false)のprocess.Start失敗パスと同じ「phaseのみ戻す」メソッドを使う。Start(clean=true)/
     Loadの失敗パスが使う`MarkStopped`〈running=falseに強制する〉とは別メソッドとして分けてある）
  6. port.GateNotifier経由で deactivate-complete 送信
```

`Start(clean=false)`は`world.WipeWorld()`・`archives.Restore()`のいずれも呼ばない点が`Start(clean=true)`/`Load`との唯一の違いであり、それ以外（`opMutex`獲得〜`process.Start()`〜`ready`待ち〜`hardcore-ready`送信）は8節の手順6〜8と完全に共通のロジックである。実装時は、この共通する後半部分を1つの非公開ヘルパー関数に切り出し、`Start(clean=true)`/`Load`/`Start(clean=false)`の3箇所から呼ぶ形にするとよい（重複を避けるため。仕様書側は`/start`（`clean`無し）を「ワールド操作を伴わない`/start clean`・`/load`の共通部分」と位置付けている、`specification.md` 2.1節）。

`Deactivate`は`evacuate-request`を伴う点が`Start(clean=true)`/`Load`と共通しており、こちらも同様の共通化が可能（`evacuate-request`送信〜`evacuate-complete`待ち〜`process.Stop()`の部分）。

**Gate層での分岐**：`adapter/gateserver`は受信した`start`メッセージの`clean`フィールドを見て、`Application`インターフェース上は`Start(ctx, clean bool, name string, force bool, requestedBy string) error`のような単一メソッドに委譲するか、`clean`の値に応じて`StartClean`/`StartResume`のように分けるかは実装の好みだが、`application.ChallengeApplicationService`内部では8節・8a節の通り完全に別経路として扱う（共通しているのは末尾のプロセス起動〜`hardcore-ready`送信のヘルパーのみ）。

## 9. 設定ファイル設計（`config.yml`）

**読み込み元パス**：Managerは起動時、`--config`フラグで指定されたパスから`config.yml`を読む（例：`manager --config /etc/hardcore-together/config.yml`）。フラグ省略時のデフォルトは`./config.yml`、すなわち**Managerプロセスのカレントディレクトリ直下**（＝10節の`<Managerの作業ディレクトリ>/config.yml`）。`config.hardcore.workDir`・`config.archive.dir`のような相対パス設定は、この`config.yml`自体の位置ではなく、常にManagerプロセスのカレントディレクトリ基準で解決する（設定ファイル自体の置き場所と紐付けて特別扱いはしない、単純な仕様）。

Docker運用時（11節）は、Dockerfileの`WORKDIR`を`<Managerの作業ディレクトリ>`に固定し、そこへ`config.yml`をイメージへ焼き込むかVolumeでマウントするかのどちらかにする想定。コンテナ内では常に同じ絶対パスになるため、`--config`は指定せずデフォルトの`./config.yml`のままで動く。

```yaml
signalPort: 9001                       # MOD⇔Manager、127.0.0.1限定リッスン（docs/protocol-mod-manager.md）
gateListenAddr: "0.0.0.0:9000"         # Gate⇔Manager、Docker network内限定を想定（docs/protocol-gate-manager.md）

state:
  path: "./state.json"                 # running値の永続化先（2節）。Manager自身の再起動をまたいで復元する

hardcore:
  workDir: "./hardcore"                # Managerがos/execで起動する子プロセスの作業ディレクトリ
  startCommand: ["java", "-jar", "server.jar", "nogui"]
                                        # world/生成時のhardcore固定・シードは<workDir>/server.propertiesの
                                        # hardcore=true・level-seed=（空）で制御する（3節、templateDirは廃止）
  recordsDir: "records"                # hardcore MOD設定のstorage.recordsDirと必ず一致させること（5節）
  pidFile: "./hardcore.pid"            # 孤児プロセス検知用（3節）。Manager起動時、この中のPIDが
                                        # 生きていれば道連れになれなかった旧プロセスとみなし終了させる

archive:
  dir: "./archive"                     # archive/<name>/ の保存先（4節）

timeouts:
  evacuateCompleteSeconds: 30          # 要確定（14節）
  hardcoreReadySeconds: 120            # 要確定（14節）
  processStopSeconds: 30               # SIGTERM→SIGKILLエスカレーションまでの猶予（3節）
```

`admins`（OP UUIDリスト）や`velocitySecret`のようなプレイヤー・権限関連の設定はGate側の責務であり、Managerの`config.yml`には含めない（仕様書1節：Managerはファイル操作・プロセス管理・記録読み取りに徹する）。

## 10. ディレクトリ構成

仕様書11節の構成そのままで、テンプレート用ディレクトリは持たない（3節の通り、ワールド生成はNeoForge自身に委ねるため）。

```
<Managerの作業ディレクトリ>/
├── config.yml
├── state.json                        … runningの永続化先（2節、config.state.path）。Manager自身の
│                                        再起動をまたいで挑戦の進行状態を復元するために使う
├── hardcore.pid                      … 現在起動中のhardcore子プロセスのPID（3節、config.hardcore.pidFile）。
│                                        プロセス起動中のみ存在し、正常停止時に削除される
├── archive/
│   └── <name>/
│       ├── world/
│       └── meta.json
└── hardcore/                        … config.hardcore.workDir
    ├── world/                        … /startで削除・再生成される（新規生成時、シードは都度ランダム）
    ├── records/
    ├── server.properties             … hardcore=true・level-seed=（空）をManagerが/start時に保証する（3節）
    ├── mods/, config/ 等
```

## 11. Docker構成

| | 内容 |
|---|---|
| 公開ポート | 無し（`signalPort`は`127.0.0.1`限定、`gateListenAddr`はDocker network内限定でホストへは公開しない） |
| Volume | `archive/`・`hardcore/`・`state.json`（永続化が必要。コンテナ再作成時もアーカイブ・進行中の挑戦・`running`の値を失わないため。特に`state.json`はコンテナ再作成時にもデッドロック〈2節〉を再発させないために必須）。`hardcore.pid`（3節）は同じ作業ディレクトリに置かれるため自然と同じVolumeに乗るが、コンテナ再作成時は中身のプロセスごと消えるため永続化は必須ではない（孤児プロセス検知はあくまで「コンテナは生きたままManagerだけが再起動した」場合のための仕組み） |
| ベースイメージ | Manager自体（Goバイナリ）に加え、hardcoreサーバー実行に必要なJavaランタイムを同一イメージに含める必要がある（`os/exec`で直接`java`を起動するため） |
| ネットワーク | Gateからの制御プロトコル接続（`gateListenAddr`）のみ外部（同一Docker network内）に露出。hardcoreサーバー自体のMinecraftポートはGateからのみ到達可能であればよく、ホストへの公開は不要 |

## 12. 並行性・排他制御まとめ

| ロック | 保護対象 | 種類 |
|---|---|---|
| `adapter/fsstate.Repository`の`RWMutex` | `{phase, running}` | 読み取り頻度が高い（`state-query`）ため`RWMutex`。`running`変化時のディスク書き込み（2節）はこのロックを保持したまま行う（書き込み中に別の読み取りを許すと、メモリ上の値とディスク上の値が一瞬食い違うウィンドウができるため） |
| `application.ChallengeApplicationService.opMutex` | プロセス再起動シーケンス（`Start`〈`clean`の有無を問わず〉/`Load`/`Deactivate`）とアーカイブコピー（`HandleArchiveRequest`）の排他 | ブロッキング`sync.Mutex`（8節・8a節・4節）。レイヤー分割前は`modserver`とも共有する`*sync.Mutex`だったが、`HandleArchiveRequest`をapplication層に集約したことで完全に内部化された（1節） |

`DecideStart`自身が持つ「`phase`が`starting`/`stopping`なら`force`/`clean`の値に関わらず即座に拒否する」ガード（2節）が「遷移中の多重`/start`・`/load`・`/deactivate`」を自然に弾くため、`opMutex`とは別に「起動処理中フラグ」を用意する必要は無い。これは`running=unknown`頼みの仕組みではない点に注意——`stopping`（`/deactivate`中）では`running`は`unknown`にならず直前の値のままだが、`DecideStart`が`phase`自体を直接見るため、`clean:true`（`running`チェックを免除する指定）で来た多重呼び出しもこのガードでは免除されず正しく弾かれる。`opMutex`自体も`clean`・`force`で免除しない（仕様書2.1節「`force`の適用範囲」：アーカイブ実行中の排他制御は`clean`・`force`でも免除しないことと整合）。`start{clean:false}`・`deactivate`は`TryMarkResuming`/`TryMarkDeactivating`がそれぞれ`phase`の排他的な検査兼コミットを行うため、これも専用のロックを別途必要としない。

## 13. テスト戦略

hardcore MOD・Gate本体が別リポジトリのため、実MOD・実Gateを繋いだe2eテストはこのリポジトリ単体では組めない。`docs/protocol-mod-manager.md`・`docs/protocol-gate-manager.md`を正としたGoのテストで代替する（`go test ./... -race`で完結、Docker不要）。レイヤーごとにテストの性質が異なる：

- **`domain/*`**：純粋関数のみなので、一時ディレクトリもTCP接続も使わず、Goの値だけでテストできる（`DecideStart`・`ResolveName`・`DecideBaseName`・`AggregateSaveData`・`AggregateSenpan`）
- **`adapter/fsstate`**：並行`TryMarkStarting`呼び出しのうち1件だけが成功することを検証する並行性テストを含む（`-race`必須）。加えて、一時ディレクトリ上で①永続化ファイルが存在しない状態から構築すると`running=false`・`phase=stopped`で初期化されること、②`SetRunning`/`MarkReady`後に別インスタンスとして再構築（Manager再起動を模す）すると直前の`running`値が復元されること、③`MarkUnknown()`はディスクへ書き込まないこと、を検証する（2節のデッドロック修正の回帰テスト）
- **`adapter/osprocess`**：実際のMinecraftサーバーJarの代わりに`sh -c`の簡易スクリプトを`startCommand`に指定し、起動/`SIGTERM`停止/タイムアウト後の`SIGKILL`エスカレーションを検証する。加えて、PIDファイルによる孤児プロセス検知（3節）の回帰テストとして：①`process.Start()`後にPIDファイルが書かれ`process.Stop()`後に削除されること、②生存中のプロセスのPIDを書いたPIDファイルを用意した状態で`Runner`を新規構築すると、そのプロセスへ`SIGTERM`/`SIGKILL`が送られ終了してからPIDファイルが削除されること、③既に終了しているPID（または存在しないPID）を指すPIDファイルがあっても誤って何かを終了させようとしないこと、を検証する
- **`adapter/fsarchive`・`adapter/fsrecords`**：一時ディレクトリ上で実際のファイルI/Oを検証する
- **`adapter/modserver`・`adapter/gateserver`**：`net.Listen("tcp", "127.0.0.1:0")`で実TCPサーバーを起動し、テストコード側がNDJSONクライアントとして接続して往復を検証する。`Application`インターフェースはこの2パッケージそれぞれにフェイク実装を用意し、プロトコル層のテストが業務ロジック（application層）の実装に依存しないようにしている
- **`application`**：`port.*`各インターフェースのフェイク（`fakeGate`・`fakeReady`・`fakeProcess`・`fakeWorld`・`fakeArchive`・`fakeRecords`・`fakeClock`）＋実物の`adapter/fsstate.Repository`を組み合わせ、`Start`（`clean`両パターン）/`Load`/`Deactivate`の一連シーケンス（8節・8a節）・各失敗パスでのstate復旧・`opMutex`によるアーカイブとの排他を検証する。既に起動中の状態で`Start(clean=false)`を呼ぶと拒否されること、遷移中（`starting`/`stopping`）は`clean`の値に関わらず「処理中です」で拒否されること、`Start(clean=false)`は`fakeWorld`のワイプ/復元メソッドを一切呼ばないこと（`world/`の有無も判定に使わない）、`Deactivate`成功後も`running`値が変化しないことも検証する
- **`internal/e2e`（真のE2E、`cmd/manager`のブラックボックステスト）**：上記までは全て「1層をfakeで固めて検証する」統合テストだが、`internal/e2e/e2e_test.go`だけは唯一、`cmd/manager`を`go build`で実際にビルドし、サブプロセスとして起動し、実TCP接続でGate役として接続して検証する。hardcore役も`cmd/fakehardcore`という専用の小さなヘルパーバイナリ（同じくこのリポジトリの`cmd/`配下、製品には含めない）を実際にサブプロセスとして起動し、MOD⇔Manager間のNDJSONプロトコルを実際にしゃべらせる。検証内容：
  - `state-query`→`start`（force無し、拒否）→`start`（force有り）→`evacuate-request`/`evacuate-complete`→`hardcore-ready`→`state-query`（ready/true確認）
  - `server.properties`の`hardcore=true`強制が実際に反映されていること
  - `cmd/fakehardcore`が`ready`直後に送る`archive-request`が実際に`archive/`配下へファイルとして残ること
  - `/load latest force`実行後、`world/`の中身が**アーカイブ時点の内容と一致する**こと（`archive.Restore`前に`world/`を消し忘れるとこの比較が失敗する。4節・8節で述べた実装バグの回帰テストそのもの）
  - `SIGTERM`送信で`cmd/manager`自身だけでなく子プロセス（`cmd/fakehardcore`）も終了すること（graceful shutdownの回帰テスト）
  - **デッドロック修正の回帰テスト（2節）**：`cmd/manager`を`SIGTERM`で終了させたあと`state.json`が残った状態で**再度起動し直し**、真新しい`state-query`が（`unknown`ではなく）直前の`running`値を返すこと。さらに、`state.json`が存在しない状態（初回起動）から起動した`cmd/manager`に対して`start`（`force`無し）がいきなり受理されること（旧設計なら`unknown`＝拒否になっていたシナリオ）
  - **孤児プロセス検知の回帰テスト（3節）**：`cmd/manager`を`SIGKILL`で強制終了させる（`SIGTERM`を送らず、グレースフルシャットダウンを経由させない）。この時点で`cmd/fakehardcore`は生き残ったまま、`hardcore.pid`ファイルも残る。この状態で`cmd/manager`を再度起動し、①旧`cmd/fakehardcore`プロセスが実際に終了すること、②その後受け付けた`start`（`clean`無し）が新しい`cmd/fakehardcore`を問題無く起動できること（＝二重起動が起きていないこと）を検証する

  `go build`とサブプロセス起動を伴うため他のテストより遅い（それでも1秒未満）が、ビルドタグやスキップ条件は付けず`go test ./...`で常に実行する。これは「手動でバイナリを起動してGate役・MOD役のTCPクライアントを繋いで確認する」という開発中に何度も行った手動E2E検証（4節・8節の実装バグはこの手動検証で発見した）を、使い捨てず自動化して恒久的な回帰テストにしたもの。
- **手動E2E確認**：上記に加え、`cmd/manager`から実バイナリをビルドし、Gate役・MOD役のTCPクライアント（Python等）を接続して`/start force`→アーカイブ→`/load latest force`→SIGTERM終了までの一連の流れを実際に動かして確認した。ユニットテストだけでは検出できなかった実装バグ（4節参照）はこの手動E2Eで見つかっている

## 14. 未確定事項・要確認ポイント（Manager側、実装着手前に確定させたい）

1. **`hardcore/`作業ディレクトリの初期セットアップ手順**（3節・10節）：`server.properties`・`mods/`・`config/`は仕様書11節で「本仕様の対象外」とされている標準NeoForgeサーバー構成だが、初回に誰が用意するか（Dockerイメージへ焼き込むのか、初回起動時にManagerが雛形を生成するのか）は未確定。3節の「Managerが`hardcore=true`を保証する」処理も、この初期ファイル一式が既に存在すること前提であり、真っさらな状態からの自動セットアップまでは範囲に含めていない
2. **`evacuate-complete`待ち・`ready`待ちのタイムアウト秒数**（8節・9節）：Gate側の`architecture-gate.md`にも関連する未確定事項があり、双方のリポジトリで値を揃える必要がある
3. **Gate⇔Manager間の接続タイムアウト・リトライ回数**（`docs/protocol-gate-manager.md` 5節と共通）
4. **MOD⇔Manager間の接続リトライ回数・バックオフ設定値**（`docs/protocol-mod-manager.md` 7節と共通）
5. **`archive-request`拒否の即時通知**（`archive-rejected`案、仕様書10節・`docs/protocol-mod-manager.md` 7節と共通の未決事項）：現状MOD側は`archive-complete`のタイムアウト（目安60秒）でしか失敗を検知できない
6. ~~**Manager障害時の再接続後の再同期手順**~~：**解決済み（2節・3節・8a節）**。Manager自体がクラッシュ→再起動した場合、`os/exec`の子プロセス（hardcore）への再アタッチはサポートしない——`phase`は常に`stopped`で再初期化し、ユーザーが明示的に`/start`（`clean`無し）を呼んで新しい子プロセスを起動し直す。挑戦の進行状態（`running`）だけは`state.json`（2節）から復元するため、進行中の挑戦を`/start`一発で（ワールドを破棄せず）再開できる。**ただしこれは`SIGTERM`によるグレースフルシャットダウンの場合の話であり**、Manager自体が`panic`・OOM Kill・`SIGKILL`で即死し子プロセスが孤児として生き残るケースは、3節のPIDファイルによる生存確認・強制終了で別途対処する
7. **`docs/protocol-gate-manager.md`・`docs/protocol-mod-manager.md`の変更フロー**：3リポジトリ（Gate・Manager・hardcore MOD）間でプロトコル定義をどう同期するか
8. **`state.json`の書き込み失敗時の扱い**（2節）：ディスクフル・権限エラー等で永続化書き込みが失敗した場合、`SetRunning`/`MarkReady`自体を失敗させてオンメモリの状態もロールバックするか、オンメモリだけは更新してログ警告に留めるか未確定
9. **PIDファイルの生存確認の具体的な実装**（3節）：PID再利用による誤検知対策として`/proc/<pid>/cmdline`等でのコマンド照合まで行うか、単純なシグナル送信によるプロセス存在確認だけに留めるかは未確定。いずれもLinux（Docker運用、1節）を前提とした実装になる

（`Deactivate`のプロセス停止は3節の`process.Stop()`をそのまま再利用するため、`processStopSeconds`〈9節〉のタイムアウト設定も共用でき、新規の未確定事項にはならない）

## 変更履歴

- 初版：`specification.md`・`docs/protocol-gate-manager.md`・`docs/protocol-mod-manager.md`を踏まえ、Manager側のパッケージ構成・状態管理・プロセスライフサイクル・アーカイブ実行・records読み取り・2本のTCPサーバー・orchestrator・設定ファイル・Docker構成・排他制御・テスト戦略を設計。仕様書に明記の無かった「アーカイブ名重複の手動/自動判別」「セーブテンプレートの出自」をManager側の設計判断として明文化し、未確定事項に追加した
- 改訂：ワールド生成方式を変更。事前に焼き込んだテンプレートワールドをコピーする方式（`templateDir`）を廃止し、**`/start`のたびにNeoForge自身へ新規ワールドを生成させ、シード値は都度ランダムにやり直す**方式にした。hardcoreモード・難易度HARDの固定は、テンプレートではなく`hardcore/server.properties`の`hardcore=true`（バニラ標準機能、ランタイムAPI不要）で行い、Managerは`/start`時にこの値が外れていないか保証する（3節・9節・10節）。これに伴い14節の未確定事項も「テンプレートの出自」から「`server.properties`等の初期セットアップ手順」へ差し替えた
- 追記：`config.yml`の読み込み元パスを明記。Managerは`--config`フラグ（デフォルト`./config.yml`、＝プロセスのカレントディレクトリ直下）で指定されたパスを読む。Docker運用時はコンテナの`WORKDIR`を固定することでデフォルト値のまま運用できる（9節）
- 実装：`cmd/manager`・`internal/{config,state,process,archive,records,ndjson,modserver,gateserver,orchestrator}`一式を実装し、`go build`・`go vet`・`go test -race`が通ることを確認。加えて実バイナリを起動し、Gate役・MOD役のTCPクライアントで`/start force`→アーカイブ→`/load latest force`→SIGTERM終了までエンドツーエンドに動作確認した。この過程で本ドキュメントの2箇所を実装に合わせて修正：
  - **8節の疑似コードの手順順序を修正**：opMutexをrunningチェックより先に獲得する当初案だと、先発の`/start`がシーケンス全体を終えるまでopMutexを握り続けるため、後発の`/start`は2節が主張する「即座に拒否」ではなく長時間ブロックされてしまう矛盾があった。`state.TryMarkStarting`（ロック不要のアトミック処理）を先に行い、opMutexは実際のファイル/プロセス操作の直前でのみ獲得する順序に改めた。あわせて、仕様書には無かった失敗時のstate復旧方針（どこまで進んだかに応じて直前状態へ戻す／`unknown`／`stopped`を使い分ける）を明文化した
  - **`Load`のワールド削除漏れを修正**：`archive.Restore`は`os.CopyFS`でコピーのみを行い既存ファイルを上書きしないため、`Start`同様に事前の`world/`削除が必須だったが、当初`orchestrator.Load`の実装でこの呼び出しが漏れており、`/load`実行時に`file exists`エラーで失敗する実バグがあった（ユニットテストではArchiveStoreをモックしていたため検出できず、実バイナリでのE2E確認で発見）。8節・4節に`process.WipeWorld`呼び出しを明記して修正した
- **【再構成】レイヤードアーキテクチャ（domain/port/application/adapter）へ移行**：機能ごとのパッケージ分割（`config`/`state`/`process`/`archive`/`records`/`modserver`/`gateserver`/`orchestrator`）で実装した後、兄弟リポジトリ`hardcore-together-neoforge`の構成・用語（`domain`・`port.ChallengeState`・`ChallengeApplicationService`・`adapter/neoforge`）に合わせてports-and-adaptersへ再構成した。対応関係：
  - `domain/challenge`・`domain/archive`・`domain/records`：各パッケージから純粋なルール・値のみを抽出（`DecideStart`・`ResolveName`・`DecideBaseName`・`AggregateSaveData`・`AggregateSenpan`）。I/Oを一切持たないため、一時ディレクトリ・TCP接続無しでユニットテストできる
  - `port`：`ChallengeState`・`ProcessRunner`・`WorldPreparer`・`ArchiveRepository`・`RecordsRepository`・`GateNotifier`・`ReadyWaiter`・`Clock`の8インターフェースに集約（旧`orchestrator`・`gateserver`パッケージ内に散らばっていたものを集約）
  - `adapter/{memstate,osprocess,fsarchive,fsrecords,systemclock,modserver,gateserver,config}`：旧`state`/`process`/`archive`/`records`/`modserver`/`gateserver`/`config`のI/O部分がそれぞれ対応
  - `application.ChallengeApplicationService`：旧`orchestrator`の`Start`/`Load`に加え、旧`modserver`が直接行っていた`archive-request`処理（`HandleArchiveRequest`）・`ready`/`running-changed`/切断処理（`HandleReady`/`HandleRunningChanged`/`HandleDisconnect`）、旧`gateserver`が直接行っていた`savedata-query`/`senpan-query`処理（`SaveData`/`Senpan`）を統合。結果として`modserver`・`gateserver`は業務判断を一切持たない薄いプロトコルアダプタになった
  - `internal/ndjson`は業務的な層分けの外側にある共有ユーティリティとしてそのまま維持
  - `application`↔`adapter/modserver`・`adapter/gateserver`間の相互参照は、`NewServer`を`Application`無しで構築し`SetApplication`で後から注入する二段階構築で解消（循環importを避けるため、双方とも相手のパッケージを直接importしない。1節参照）
  - 再構成後も`go build`・`go vet`・`go test ./... -race`が全て通ることを確認し、実バイナリでの`/start force`→アーカイブ→`/load latest force`→SIGTERM終了のE2E確認も再実施して同じ結果になることを確認した
- **追記：手動E2E検証を`internal/e2e`として恒久化**：レイヤー分割後の再確認まで手動E2Eは`/tmp`上の使い捨てスクリプト（フェイクhardcoreバイナリ・Python製Gate/MODクライアント）で行っていたが、再現性のため`go test`の一部として書き直した。`cmd/fakehardcore`（テスト専用のMOD⇔Manager最小実装スタブ、製品には含めない）を新設し、`internal/e2e/e2e_test.go`が`cmd/manager`・`cmd/fakehardcore`双方を`go build`で実ビルドしてサブプロセスとして起動し、実TCP接続でGate役として一連の操作（拒否→強制start→アーカイブ→`/load latest force`→SIGTERM）を検証する。ビルドタグ等は付けず`go test ./...`に含めて常に実行する（13節）
- **リネーム**：`challenge.Snapshot`→`challenge.State`、`port.ChallengeState`→`port.ChallengeStateRepository`。`ArchiveRepository`・`RecordsRepository`と名前の付け方を統一するため。値そのもの（`{Phase, Running}`のペア）を指す名詞は`State`、それを読み書きする窓口（インターフェース）は他の2つと同じ`Repository`接尾辞に揃えた。`Snapshot()`というメソッド名自体は変更していない（「ある瞬間の複製である」という意味を保つため、型名とは独立に残した）。あわせて、`ChallengeStateRepository.Restore`が`ArchiveRepository.Restore`と同名で意味の異なる操作になっている点、および`TryMarkStarting`成功後アーカイブ存在チェックが完了するまで`opMutex`を獲得していない`Load`には、後発の`force`付き呼び出しが古い`prior`スナップショットで正しい状態を上書きしうる狭い競合が残っている点は、リネームとは別の課題として未対応のまま次回以降へ持ち越した
- **追記リネーム**：インターフェース名を統一した流れで、実装側の具象型名・フィールド名も揃えた。
  - `adapter/memstate.Store`→`adapter/memstate.Repository`：`fsarchive.Repository`・`fsrecords.Repository`（それぞれ`ArchiveRepository`・`RecordsRepository`の実装）と同じく、「パッケージ名が実装方式（memstate＝インメモリ）を表し、型名は`Repository`で統一する」というパターンに揃えた
  - `application.Deps.Archives`→`Deps.Archive`：`Deps`の各フィールド名は対応するインターフェース名から末尾の役割接尾辞（`Repository`・`Runner`・`Preparer`・`Notifier`・`Waiter`）を除いた形に揃えているが、`Archives`だけ複数形になっていて`ArchiveRepository`（単数）と食い違っていたため`Archive`に統一した（`Records`は`RecordsRepository`自体が複数形の単語なので元々一致している）
  - 一方、`osprocess.Runner`（`ProcessRunner`と`WorldPreparer`の両方を実装）・`modserver.Server`／`gateserver.Server`（それぞれ`ReadyWaiter`／`GateNotifier`を実装しつつ`Listen`・`Serve`・`SetApplication`というport外の公開APIも持つ）は、portの名前に合わせた改名はしなかった。これらは特定のportを実装することが主目的ではなく、より広い責務（プロセス管理、TCPサーバー）を持つ具象型が"たまたま"portも満たしている、という関係だと判断したため
- **【バグ修正】初回`/start`が永遠に成功しないデッドロックを修正**：`go test ./... -race`・実バイナリE2Eまで確認済みの実装に、一度もhardcoreを起動したことが無い状態からでも`start`が永遠に拒否され続けるデッドロックが見つかった。原因は2節「安全側デフォルト」——Manager起動直後・MOD⇔Manager接続断時は無条件で`running=unknown`とし、`unknown`は`true`と同じく`start`/`load`を拒否する、という設計。`running`キャッシュを`true`から書き換えられるのはhardcore自身が`ready`を送ってきた場合のみだが、そのhardcoreを起動させる`start`自体が拒否され続けるため、一度も起動していない状態（`unknown`が「危険側」ではなく本来「安全に許可してよい」はずの状態）でも抜け出せなかった。**Managerは`os/exec`によるプロセス起動/停止処理を既に持っており**（3節`adapter/osprocess`）、起動する主体が無かったわけではない——問題は`running`がオンメモリでManager自身の再起動のたびに無条件`unknown`にリセットされていたことだった（`specification.md` 2.1節「背景」・9節決定ログに同じ説明あり）。
  - 修正1：`running`を`adapter/memstate`のオンメモリキャッシュから、**`adapter/fsstate`がローカルディスクへ永続化する値**に変更した（`adapter/memstate`は`adapter/fsstate`へ改名。2節）。Manager起動時、永続化ファイルが存在すればその値を、存在しなければ`running=false`（`unknown`ではない）を初期値とする。`phase`は常に`stopped`で初期化する（14節旧項目6の結論：子プロセスへの再アタッチは非対応と確定）
  - 修正2：`start`メッセージに`clean`フィールドを追加し、`clean:false`はワールドに触れずプロセスを起動するだけの経路にした（旧`running`チェックを一切参照しない）。`/deactivate`コマンドも新設した（Gate側は`specification.md` 2.1節・`architecture-gate.md`参照）。当初はワールド操作を伴わない起動/停止を`activate`/`deactivate`という独立シグナルとして追加する4種構成を検討したが、「`start`と`activate`の役割分担がコマンド名から読み取れない」というレビュー指摘を受け、`start`自体に`clean`フラグを持たせる3種構成（`start{clean}`/`load`/`deactivate`）に整理し直した。これにより、プロセスがクラッシュ等で落ちた場合も、進行中の挑戦を破棄する`clean`/`force`系を使わずに`start{clean:false}`一発で再開できるようになった。加えて、`start{clean:false}`の受理条件が「プロセスが既に起動中か」（Managerが直接把握でき常に正確）だけになったため、`unknown`になりうる`running`値を参照しなくなり、デッドロックが構造的に発生しなくなった（修正1の永続化は、この時点では`load`の`running`チェックの正確性のためにのみ必要）
  - `port.ChallengeStateRepository`に`TryMarkResuming`（`start{clean:false}`専用、`running`を見ず`phase`のみで判定）・`TryMarkDeactivating`を追加、`port.WorldPreparer`に`Exists`を追加（2節・3節・8a節）。既存の`port.ProcessRunner`・`port.GateNotifier`・`port.ReadyWaiter`は`Start`/`Load`と共用し、新規のI/Oアダプタは追加していない
- **【レビュー指摘への対応】孤児プロセス対策としてPIDファイルによる生存確認を追加**：上記デッドロック修正のレビュー中、「`phase`は常に`stopped`で再初期化する」（14節旧項目6）という前提は、Manager自体が`SIGTERM`で正常終了する場合にしか成立しないという指摘があった。Manager自体が`panic`・OOM Kill・`SIGKILL`で即死した場合はグレースフルシャットダウンが走らず、hardcore子プロセスが孤児として生き残ったまま、新しいManagerが「止まっている」と誤認して`phase=stopped`で起動しうる。この状態で`start{clean:false}`が受理されると、生きている旧プロセスと同じ`world/`・同じポートへ向けて新しいプロセスを二重起動してしまう危険があった。
  - 対応：`process.Start()`成功時に子プロセスのPIDを`config.hardcore.pidFile`（例：`./hardcore.pid`）へ書き込み、`process.Stop()`の正常完了時に削除する。Manager起動時（Gate⇔Manager用TCPサーバーがリッスンを始める前）に、このPIDファイルが指すプロセスが実際に生きているかを確認し、生きていれば`SIGTERM`→`SIGKILL`で強制終了させてからPIDファイルを削除し、通常の`phase=stopped`起動を続行するようにした（3節）。これにより「Manager単体クラッシュ後の二重起動」というリスクは、Manager起動シーケンスの最初期に確実に解消されるようになった
  - PID再利用（OS再起動等で無関係な別プロセスに同じPIDが割り当てられているケース）による誤検知の可能性は残っており、コマンドライン照合をどこまで厳密に行うかは14節の未確定事項として追加した
- **【バグ修正】`/start`（`clean`無し）の誤ったワールド存在チェックを削除**：上記デッドロック修正の実装（`port.WorldPreparer.Exists()`・8a節の`start{clean:false}`手順1）はdocker compose環境での手動確認で「`world/`が存在しない状態（真新しいデプロイ）で`/start`を送ると`start-rejected`『ワールドが存在しません』になり、`/start clean`か`/load`を使わないと一度も起動できない」という不具合として発覚した。原因は設計段階のドキュメント間の食い違い：`specification.md` 2.1節のコマンド表・状態別挙動表（状態①）は一貫して「`/start`（`clean`無し）は`world/`が無ければ拒否ではなく新規作成して起動する」と明記していたにもかかわらず、本ドキュメント（`architecture-manager.md`）8a節の疑似コードと`docs/protocol-gate-manager.md`のプロトコル定義には、これと矛盾する「`world/`が無ければ`start-rejected`『ワールドが存在しません』」という誤った拒否分岐が書かれており、実装はこの誤った記述の方に従っていた。一次仕様（`specification.md`）を正として、`application.ChallengeApplicationService`から`World.Exists()`の呼び出し（および対応する拒否分岐）を削除し、`start{clean:false}`は`world/`の有無を一切見ずにプロセスを起動するだけにした（`world/`が無ければhardcoreプロセス自身が新規生成する、3節）。`port.WorldPreparer.Exists()`・`adapter/osprocess.Runner.Exists()`自体は今回のバグ修正では削除せず、未使用のまま残っている。あわせて、`Deactivate`の成功パス・`Start(clean=false)`のプロセス起動失敗パスが使う状態遷移メソッドを、`MarkStopped`の呼び分けではなく独立した`MarkDeactivated`（`running`を変更せず`phase`のみ`stopped`に戻す）として実装し直し、本ドキュメント（2節・8a節）もそれに合わせて更新した。本ドキュメント・`docs/protocol-gate-manager.md`・`specification.md`自体に残っていた関連する不整合（`/start clean`・`/deactivate`が遷移中でも拒否されうる点の記述漏れ等）もあわせて修正した
