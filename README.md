# fuserd — FUSER community web services

`fuserd` is a Go reimplementation of FUSER's web-services REST API (the "Hype backend"). It lets owners of the game
use its online features again: sign-in, multiplayer matchmaking, the Main Stage, the Diamond Store, profiles,
following, invites and profile pictures. It also adds features of its own: a community song catalogue, peer-to-peer
connectivity, and a WebAuthn-protected administrator panel.

Every endpoint path the game client knows is routed. Endpoints without an implementation answer `{"results":{}}`
and log a `stub endpoint` warning, so a client session shows which ones it calls.

## Contents

- [Build](#build)
- [Running](#running)
- [Pointing the game at the server](#pointing-the-game-at-the-server)
- [Authentication](#authentication)
- [Multiplayer matchmaking](#multiplayer-matchmaking)
- [Public game servers](#public-game-servers)
- [Main Stage](#main-stage)
- [Community song catalogue](#community-song-catalogue)
- [Community songs on Google Drive](#community-songs-on-google-drive)
- [Peer-to-peer connectivity](#peer-to-peer-connectivity)
- [Profile pictures](#profile-pictures)
- [Administrator control panel](#administrator-control-panel)
- [Logging](#logging)
- [Command reference](#command-reference)
- [Repository layout](#repository-layout)

## Build

`fuserd` needs Go 1.26 or newer; `go.mod` pins the `go1.26.9` toolchain, which a newer `go` fetches on its own
(`GOTOOLCHAIN=auto`). Module checksums are committed in `go.sum`. It's built with `CGO_ENABLED=0` (SQLite is the
pure-Go `modernc.org/sqlite`), so the binary needs no system libraries.

```sh
go build -o fuserd ./cmd/fuserd
go test ./...
```

To build a Linux binary with Docker:

```sh
docker build --output type=local,dest=build .
# binary: build/fuserd
```

The Dockerfile only exports the binary as a build artifact. It doesn't produce a runtime image.

## Running

```sh
cp config.example.json config.json   # then set token_secret (32+ random characters)
./fuserd -config config.json
```

`config.example.json` lists every setting with its default. Relative data paths (the catalogue, song files, mod
pack, DLC manifest, profile pictures and Main Stage images) are resolved against the config file's directory;
`database_path` and `log_file` are relative to the working directory.

### systemd

[`fuserd.service`](fuserd.service) runs `/opt/fuserd/build/fuserd` with `/opt/fuserd/config.json`. To deploy it:

1. Copy the binary, config and data files into `/opt/fuserd`.
2. Create a `fuserd` system user and group.
3. Give that user write access to `/opt/fuserd`, for the SQLite database and the log file.

The unit adds the user to the `docker` group so `fuserd` can start and stop game-server containers. Docker socket
access is equivalent to root on the host, so use a dedicated account. If public game servers are disabled, remove
`Requires=docker.service` and `SupplementaryGroups=docker`. Adjust the paths if you deploy elsewhere.

```sh
sudo cp fuserd.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fuserd
journalctl -u fuserd -f
```

The unit stops the service with SIGINT, the signal `fuserd` handles for a clean shutdown. It allows 120 seconds for
managed containers to stop.

### HTTPS certificates

`fuserd` terminates TLS itself. Either point `tls_cert` / `tls_key` at a certificate you renew yourself, or set
`acme.enabled` and let `fuserd` obtain and renew one from Let's Encrypt (or another ACME CA):

```json
"listen": ":443",
"public_url": "https://fuser.example.org",
"acme": { "enabled": true, "domains": [], "email": "you@example.org", "cache_dir": "data/acme", "directory_url": "" }
```

- **Challenge.** `fuserd` answers the CA's `tls-alpn-01` challenge on its own TLS listener. It needs no port 80,
  but the CA always connects to port 443 of each domain, so either listen on `:443` or forward public port 443 to
  `listen`. On Linux, binding `:443` as the `fuserd` user needs `AmbientCapabilities=CAP_NET_BIND_SERVICE` (commented
  out in `fuserd.service`).
- **`domains`** defaults to the host of `public_url`, plus the host of `control.origin` when the control panel is
  enabled. Both hosts must be listed if you set it yourself. IP addresses and wildcards aren't supported.
- **`cache_dir`** (relative to the config file) keeps the account key and certificates. Keep it across
  redeployments: Let's Encrypt allows only 5 certificates for the same names per week.
- **`directory_url`** is empty for Let's Encrypt production. Try a new setup against staging first,
  `https://acme-staging-v02.api.letsencrypt.org/directory`; its certificates aren't trusted by the game, so empty
  `cache_dir` before switching back.
- **Renewal** happens in the background once a third of the certificate's lifetime remains (at most 30 days before
  expiry). The log shows `acme certificate ready` at startup, `acme certificate obtained` on each issue or renewal,
  and a daily `acme certificate renewal overdue` warning if renewal falls behind.
- A client that connects by IP address, without SNI, gets the certificate of the first domain.

Enabling `acme` accepts the CA's terms of service. `acme` and `tls_cert` can't be used together, and `public_url` must
be `https`.

## Pointing the game at the server

Each player adds this to `%localappdata%\Fuser\Saved\Config\WindowsNoEditor\Game.ini`, with the server's public
address:

```ini
[/Script/Hype.BackendServerMgrSettings]
EnvironmentSettings=((Live, (ClientURL="https://<server>/api/")))
```

The trailing `/api/` matters. The shipping `DefaultGame.ini` maps its `Local` environment to
`http://127.0.0.1:8000/api/`, which is why `:8000` is the default listen address. For a server with a self-signed
certificate, the console variable `n.VerifyPeer=0` relaxes the client's certificate check.

### Landing page

`fuserd` serves a landing page for players at `/`. It has four tabs:

- **Server Status** (the default): public lobbies with their venue names and venue images for each host's time of
  day.
- **Diamond Stage**: the current Main Stage event's card, with viewer and headliner counts from fresh host
  reports. Missing or mismatched reports show the counts as unavailable. Reload the page to update.
- **Get connected**: when the mod pack holds the player installer, a download of FUSER Online Setup (`/setup`)
  first, with three steps (run it; it finds FUSER from Steam or the Epic Games Store, points the game at this server
  and installs the mod pack; Play). Then the `Game.ini` section above for connecting by hand, built from
  `public_url` with every region URL pointing at this server and a copy button. A closing callout says what players
  miss without the mod pack and links to the Mod pack tab.
- **Mod pack** (also opened by `/#mod-pack`): what the mod pack adds and the setup download (`/setup`), with the
  zip at `/modpack` as the alternative (only the zip when the pack has no installer). The
  stock-player text, the Diamond Stage item and the crate check item follow `multiplayer.separate_stock_players`,
  `main_stage.enabled` (with public servers) and `multiplayer.max_crate_songs`.

The mod pack is whatever file `mod_pack_file` names (default `data/FUSERNativeLoader.zip`). It's opened on each
request, so you can replace it without a restart. When it's missing, the page says so and `/modpack` returns 404.
A zip that holds the player setup (`FUSEROnline-Setup.exe` from FUSERNativeLoader's `.\build.ps1 -Package`, or
`Setup-FuserOnline.ps1` in older packs) is served with `server.txt` beside it set to `public_url`, replacing any it
had, so the setup points the game at this server without asking. The stamped zip is kept in memory, so a pack over
64 MB is served as it is (the `/setup` download still carries the address).

| Path | Serves |
|---|---|
| `/modpack` | The mod pack zip (stamped with `server.txt` as above). |
| `/setup` | `FUSEROnline-Setup.exe` from the zip's root, with `server=<public_url>`, that text's length (int32, little-endian) and `FUSERCFG` appended after the PE image; the installer reads its server address from there. 404 without a mod pack or an installer in it. |
| `/modpack/version` | The installer's update check: `{"version", "size", "sha256", "url": "/modpack"}`, the loader version from the zip's root `VERSION` file and the size and SHA-256 of exactly the bytes `/modpack` serves. 404 without a mod pack or a `VERSION` in it. |

A loopback `public_url` (the default) isn't written into either download. The stamped zip, the stamped installer and
the checksum are built once per file version and `public_url`. All three count against `store.requests_per_minute`
per client address.

## Authentication

`auth/jwt/` verifies each platform ticket against its issuer's signature:

| `platform` | Ticket | Checked |
|---|---|---|
| `stm` | Steam auth session or ownership ticket (hex) | RSA-SHA1 against Valve's ticket key; app ID 1331440; SteamID; expiry. DLC app IDs are recorded for `auth/steam/owned_dlc/` |
| `epc` | Epic account ID token (JWT) | RS256 against Epic's JWKS; issuer; audience = FUSER's EOS client ID; subject = `platform_user_id`; expiry |
| `ntd` | Nintendo BaaS ID token (JWT) | As Epic, plus `nintendo.ai` = FUSER's title ID. Off by default |

The server then issues its own HS256 session token, which the client sends back as `Authorization: JWT <token>`.

### Epic emulator exemptions

Optionally, Epic logins whose signature check fails can still be admitted, but only for identities provisioned in
advance. This is off by default. A JWKS fetch failure, an expired token, a wrong issuer, audience or subject, or an
unsupported algorithm never triggers the fallback.

To use it, set `auth.allow_epic_failed_signatures` to `true` and provision an exact `sub`/`dn` pair before the
player logs in. The server may keep running:

```sh
fuserd exempt -config config.json -username '<JWT dn claim>' -key '<emulator epicid>'
```

This adds the pair as `allowed`. Running it again with the same key changes the name. A key that is already
`verified` or `blocked` is refused. The control panel's User exemptions tab does the same.

- **`sub`** is the emulator's editable `epicid`. Generate a new, unguessable one for each player and have them
  keep it private. The fallback treats it as a bearer secret, not proof of Epic ownership, so serve the API over
  HTTPS.
- **`dn`** must match the JWT's `dn` claim exactly.
- **Verified logins lock out the fallback.** A login with a valid Epic signature permanently marks its `sub` as
  `verified`, and that account can no longer use the fallback.
- **Malformed tokens are repaired.** Some emulators' base64 encoders drop the final partial 3-byte group, so the
  claims JSON loses its closing `}` or `"}`. fuserd restores those closing characters, and it also accepts `=`
  padding and the standard base64 alphabet.
- **Turning it off:** setting the flag back to `false` disables every fallback login at once, without deleting the
  allowlist.

To block a `sub` whatever its signature or state:

```sql
INSERT INTO epic_auth_policy (sub, state) VALUES ('<sub>', 'blocked')
ON CONFLICT(sub) DO UPDATE SET state = 'blocked';
```

## Multiplayer matchmaking

`multiplayer/*` serves the game's Create, Quickplay and session-browser flows. State is kept in memory, so a
restart ends every session.

**Private sessions** always run on their own [FDS container](#fuserdedicatedserver-fds). Without public game
servers configured, private tickets are refused and `private sessions unavailable` is logged at startup. **Public sessions** run on
the configured [public game servers](#public-game-servers). With none configured, players' own games host them.

### Player-hosted public sessions

These apply only when no public game servers are configured.

- **Creating a session.** A public ticket asking for a new match creates a session at the creator's address (as
  this server sees it) on `multiplayer.host_port` (7777). The creator's player session id starts with
  `psess-host-`, which tells the creator's loader to host instead of connecting.
- **Quickplay.** A public ticket without "create new" joins the newest public session, or hosts one if there is
  none.
- **Session end.** A session ends when its host cancels, starts another ticket, or stops sending `players/ping/`
  for `multiplayer.host_timeout_seconds`.
- **Addresses.** Behind a reverse proxy, set `multiplayer.trust_forwarded_for`, or every host gets the proxy's
  address. A host behind NAT must forward UDP `host_port` and `host_port` + 100. Two players behind the same NAT
  both get its public address, which only works if the router supports hairpin NAT. [Peer-to-peer
  connectivity](#peer-to-peer-connectivity) avoids port forwarding.

### Session browser and invites

- **Visibility.** `get_list` returns the sessions the viewer may see, each labelled with the viewer's relation to
  the host:
  - `invite`: the viewer holds an invite;
  - `platform_friend`: either player uploaded the other as a platform friend;
  - `followship`: either player follows the other;
  - `public`: a public session.

  Anyone can see public sessions. Private ones are visible only to the host and related players, unless
  `multiplayer.open_private_sessions` is on (a test switch): then everyone sees them, labelled `invite`.
- **Beacon check.** The client queries each listed host over a beacon on `host_port` + 100 and shows only the
  sessions that answer. Joining goes through `player_sessions/` and then `game_sessions/get/`.
- **Invites.** The invite screen lists the host's platform friends and followers and sends
  `game_sessions/invite/` for each player picked. The invitee receives it in `feed/pull_notifications/`
  (`invites[]`, polled every 10 s), with a Join action. An invite ends when the invitee joins, when it's
  cancelled, when its session ends, or after an hour.

### Profiles and social

- **Social endpoints.** Players follow each other (`followship/*`), find each other by hype username
  (`search/username/`), and view profiles (`players/get/`).
- **Display names.** In every profile, `platform_profile.platform_username` is the hype username without its
  `#NNNN` suffix; `hype_username` keeps the suffix for search. Other players' platform account IDs are never
  returned.
- **A player's own profile.** The client asks for its own profile once after login and takes its name from it.
  Without that name, the player never appears on their own machine: no performer, no player card, no names on the
  venue interstitial, no song HUD.
- **Own stats.** The own-profile answer also sets the client's disc count, matchmaking rating, battle wins and
  losses, and Elder Credit balance. fuserd stores those fields from each `players/xp/` (a `-1` keeps the stored
  value) and returns them. The balance is the player's own, or `store.elder_credit_balance` when that's set. If no
  `players/xp/` arrives within 2 s, the answer carries zeros and logs a warning.
- **Platform friends.** The friend list the client uploads (`players/platform_blocked_users_and_friends/`) feeds
  `players/platform_friends/`, matched on the same platform only.

### Blocking

`players/block/`, `players/unblock/` (`{user_id}`) and `players/blocked_users/` (paged) keep each player's block
list. Profiles carry `is_blocked` (the viewer blocked them) and `blocked_you`.

Blocked players are kept apart in both directions, whichever one blocked the other:

- Public matchmaking never places them together. It picks another server (starting one if needed) or another
  player-hosted session. A viewer gets the game's "no show" error if every show holds a blocked player.
- The session browser hides a session holding a blocked player, and a join to one is refused.
- An invite from a player the invitee blocked isn't delivered, and the inviter isn't told. Blocking also
  withdraws the blocked player's open invites.
- **An invite overrides all of this:** an invited player sees the session and may join it, whoever is in it.

A session's occupants are its members seen online in the last 2 minutes, plus players still being placed on its
FDS server. FDS reports player counts, not ids, so a player who left an FDS session still counts until they go
offline or start another match.

### Crate song limit

`multiplayer.max_crate_songs` caps the songs in the crate a player enters multiplayer or the Main Stage with, so a
session's custom songs can sync in time. `0` (the default) means no limit. Any other value must be at least 24, the
stock crate size, because stock players have no loader to enforce it.

The loader reads the limit from `multiplayer/limits/`, which needs no login because the game checks the crate
before it logs in. Above the limit, the loader fails the game's own crate check with a "Crate Error" popup naming
the limit. fuserd logs `crate song limit max_crate_songs=N` at startup when it's set.

### Stock and loader players

With `multiplayer.separate_stock_players` (default `true`), players on stock FUSER are kept apart from loader
users. A stock client can't play another player's custom songs: its queue stalls on them, and a stock late joiner
never finishes its sync.

- **Who runs the loader.** The loader reports itself by posting `{"loader", "build"}` to `multiplayer/limits/` with
  its login, at login and on each entry to multiplayer. This is logged as `loader seen player=… version=… build=…`.
  A player who never reported counts as stock. Reports are kept in memory, so after a restart a loader user counts
  as stock until their next entry to multiplayer.
- **Matchmaking.** Public matchmaking (DJ and viewer tickets) only places a player with their own group, as it
  does for blocked players.
- **Session browser.** It hides the other group's sessions and refuses a browser join to one. An invite overrides
  both.
- **The Main Stage** is for loader users only, unless the event allows stock players (see [Main
  Stage](#main-stage)).

### Not served

Battles, feed notifications other than invites, and the server-to-server API.

**Battle mode is off by default.** `players/active_battle_season/` answers the game's own `"noseason"`, so the
Battle hub stops at sign-in with the game's "battles season down" message instead of reaching matchmaking. Only
the Battle hub checks the season: multiplayer, the Main Stage, the social hub and the store are unaffected.
`battle_season.enabled: true` reports `battle_season.name` (`season1` or `season2`) as active again; battle tickets
are still refused with an error, because fuserd has no battle servers. The startup log shows
`msg="battle mode" enabled=false`.

## Public game servers

With `multiplayer.public_servers.enabled`, fuserd runs public sessions, private sessions and the Main Stage on
FuserDedicatedServer (FDS) Docker containers it starts and stops itself. It runs up to `max_servers` of them, on
ports from `base_port` in steps of `port_step` (at least 101).
`image` names the FDS container image, which is built separately.

**Placement.** A public ticket goes to a running server with the same round count, room under `max_players`
(default 11) and no blocked player. Otherwise a new server is started.

- **"Join a public session"** (a DJ ticket, Quickplay or Create) also needs a **free performer slot**: the
  server's DJs plus DJs still being placed there must be below `max_djs` (default 4, FUSER's
  `MaxNumDJsInSession`). So a fifth performer gets a new server instead of a viewer's seat. `max_djs` 0 turns the
  check off.
- **"View a public session"** never starts a server. It joins a running show with room under `max_players`.

- **Reservations.** A ticket holds a place on its server from when it's placed until its player connects. A place
  is given up 2 minutes after the server registered (or after the ticket's last poll while it was still being
  placed, if later), so a slowly booting container keeps its players' places.
- **Departures.** Each heartbeat lists the connected players. A player missing from it for 2 minutes (and placed
  more than 2 minutes ago) is taken out of the server's session, so seats that players left by quitting the game are free
  again, and the host role passes to the earliest-joined player left. Tickets still being placed are not touched.
- **Recycling.** A container past `FDS_MAX_UPTIME_HOURS` reports `recycle_pending` and exits once empty. Until then
  it takes no new tickets, viewers or browser joins, and stays listed on the website. The Main Stage keeps admitting.
- **Warm pool** (`warm_servers`, default 0 = off). fuserd keeps this many public servers up with no player and no
  reservation, started for `warm_rounds` rounds (1 to 10, default 1; 1, 2 and 3 rounds start with 32, 64 and 128
  bars), so public tickets with that round count don't wait for a container to boot. Warm servers count against
  `max_servers` and wait for players indefinitely; at most one is started every 15 s. The pool always leaves one
  slot free for servers that tickets start (and one more for the Main Stage server while it isn't running), so
  `warm_servers` is at most `max_servers` - 1, or `max_servers` - 2 with the Main Stage on. When a ticket, a private
  session or the Main Stage still finds every slot taken, an idle warm server is stopped to make room; that request
  fails, and the next one gets the slot once the container has exited.

**Launch failures.** Three containers that stop before registering within 10 minutes pause every launch (public,
private and Main Stage) for 2 minutes, doubling on each further trip up to 30 minutes; a server that registers ends
the pause. Tickets that would need a new server meanwhile get "no public server available". After a Main Stage
server fails, its restart waits 30 s, then 60 s, 120 s and 300 s on further consecutive failures; a clean exit (its
scheduled restart) or a failure after 10 minutes of running starts the count over.

**Ports.** A new server takes the first port of the plan that no server or `fuserd-*` container holds, that isn't
cooling down, and whose game port and beacon port (game port + 100) fuserd can bind over UDP. A port another
program holds is skipped for 5 minutes. A container that registers on other ports than it was given (its own were
taken), or reports 0 for one (it isn't listening on it), is stopped at once, and both of its assigned ports cool
down for 5 minutes.

**Restarts.** Containers are left running when fuserd exits, so a restart doesn't end anyone's set. On startup
fuserd lists the running `fuserd-*` containers:

- a stray Main Stage server (one fuserd didn't stop, after a crash) is stopped before the new one starts;
- a public or private container is re-adopted when it is clean: this `image`, a port of the port plan, this
  fuserd's registration URL and session-token key, and a registration token in its environment (read with
  `docker inspect`, never logged). Its heartbeats are accepted again and it takes tickets like any other server.
  Players already in it keep playing, but their old fuserd sessions are gone;
- any other `fuserd-*` container (another `image`, port plan, registration URL or `token_secret`, or another
  fuserd's on the same Docker host) is left running, since it may still have players. No new server takes its ports
  while it runs; an FDS whose heartbeats fuserd refuses exits once it is empty.

### FuserDedicatedServer (FDS)

- **`game_root`** is a FUSER install, mounted read-only at `/fuser`. FDS reads its paks.
- **The pak key** comes from fuserd's own environment, from the variable named by `fds_pak_key_env` (default
  `FDS_PAK_KEY`). With systemd, set it with an `Environment=` or `EnvironmentFile=` line. It reaches each container
  as `docker run -e FDS_PAK_KEY` with no value, so it never appears on a command line or in `config.json`.
- **Registration.** The registration API has its own plain-HTTP listener, `fds_listen` (default
  `127.0.0.1:8001`). It's separate from the public listener and never exposed through it. Host-networked containers
  reach it on loopback, so they don't depend on TLS or a NAT hairpin. `fds_register_url` overrides the URL handed
  to containers; empty means `http://<fds_listen>`. Each container calls:
  - `POST /fds/v1/register` when it's ready;
  - `POST /fds/v1/status` every `fds_heartbeat_seconds`;
  - `POST /fds/v1/exit` when it shuts down.
- **Held-back custom songs.** Every register and status reply carries `blocked_songs`: the lower-case shortnames
  of the custom songs not every member of the server's session can play. FDS (with its `bBlockUnreadyCustomSongs`,
  default on) doesn't relay those songs' SpawnDisc actions, because a stock client that gets a disc it can't load
  stops playing that DJ's actions for the rest of the set. A song is held back
  - while a member counts as stock (no loader report; see `separate_stock_players`): every custom song the members'
    loaders report holding, lacking or asking other players for;
  - while a member went without it: its loader gave up fetching it, or its transfer ran out of owners;
  - while members hold different files of it (see [song versions](#song-transfers-between-players)).

  Official songs never are. fuserd learns a session's songs from the loaders' `p2p/song/poll/`, so with `p2p.enabled`
  or `p2p.song_transfers` off the list stays empty. A change is logged as `custom songs held back on the server`.
- **Bearer tokens.** Each call carries a random per-container token, generated at launch and revoked when the
  container exits. It reaches the container only through its environment, and neither side logs it.
- **Health.** A container that doesn't register within `startup_timeout_seconds`, or misses three heartbeats, is
  stopped.
- **Venue.** A new container loads the creating ticket's venue and time of day (`FDS_VENUE`, `FDS_TIME_OF_DAY`),
  and every ticket placed on it gets that venue in `matchmaker_data`.
- **Session tokens** (`fds_session_tokens`, default `true`). The player session id fuserd hands a client for an
  FDS session is a signed token, `psess-fds1.<claims>.<signature>`. It's Ed25519-signed, valid for 10 minutes, and
  names the container's slot, the game session and the user. The signing key is derived from `token_secret`, so it
  survives restarts.
  - Each container gets the public key (`FDS_TOKEN_PUBLIC_KEY`). It refuses a login whose token is missing,
    expired, forged, issued for another container, user or game session, or already held by a connected player.
  - The client sees `invalid_player_session`. The container logs the reason, never the token.
  - Set it to `false` only for troubleshooting: containers then admit anyone who reaches the port.
- **Private sessions.** A private ticket (`freestyle_private`) gets an FDS container of its own
  (`FDS_SERVER_ACCESS=private`), never shared with public tickets or listed for viewers. Invitees join through the
  usual player-session flow and get tokens for that container. A private session takes a slot from `max_servers`
  like any public one.

## Main Stage

With `main_stage.enabled`, fuserd serves the Main Stage: a schedule of events where booked headliners take turns
as DJs in front of an audience. It needs [FDS](#fuserdedicatedserver-fds) public game servers. fuserd keeps one
persistent Main Stage container running in one of their slots, and restarts it within 15 s if it stops.

**The schedule** is a fixed rotation plus stored events:

- The rotation (`main_stage.rotation`) runs an event every `interval_minutes` from `anchor`. Rotation events get
  stable UUIDs, so headliners booked on them survive restarts.
- Stored events are added with `fuserd mainstage` or the control panel. A stored event, maintenance included,
  replaces every rotation event it overlaps. Stored events can't overlap each other.
- An event lasts `event_seconds` (default 1750, FUSER's own Main Stage length) and has `slots` headliner slots
  (default 4). The Main Stage server splits the event's time evenly between the headliners present, in whole
  32-bar blocks. `turn_minutes` (default 0, no limit) caps each turn.

**Booking a slot.** Players claim headliner slots in the game, first come, first served. Slots are free.

- Slots can be claimed from `purchase_open_minutes` (120) until `purchase_close_minutes` (5) before an event
  starts.
- A player may hold one slot per event. A player who headlined one of the last `cooldown_events` events can't
  claim another (default 1; 0 = no cooldown).
- A slot is refused when the buyer and a booked headliner have blocked each other.
- Operators can assign any player to any slot with `fuserd mainstage assign` or the control panel. Assignments
  ignore the purchase window, the cooldown and blocks.

**Joining.** The game's Main Stage join carries no session token, so `main_stage/servers/` gives the caller a pass
valid for `pass_minutes`. The Main Stage server checks every login against fuserd's passes
(`POST /fds/v1/mainstage/admit`) and kicks players without one. With `pass_check_ip`, the login must come from the
address that asked. The server holds `max_players` (default 50); beyond that, the client shows its own "Main Stage
full" error. The server loads `venue` (default `VenueB`, the game's Main Stage venue).

**Stock players.** Only loader users may join or headline, unless the event allows stock players: `-allow-stock`
on a stored event, or `main_stage.rotation.allow_stock` for rotation events. The current event decides (between
events, the next one). A stock player already in stays. A refused stock player sees the client's generic
matchmaking error.

**Event images.** With `generated_images` (default `true`), fuserd draws each event's card from its headliners: one
tile per booked slot with the headliner's profile picture and name, plus the open slots while they can still be
claimed. Cards are re-rendered whenever a slot or a headliner's picture changes, stored in `image_dir` and served
from `/api/mainstage/image/`. An event's or the rotation's `image_url` replaces the generated card.

**Between sets.** The game's Diamond Stage tiles (main menu and hub) show the *current* event. With no current event
they show no text and a stale image or a spinner. So between sets fuserd tells the game that the last event that
ended is still running, until the next event starts. The rules:

- The hold only happens when no event is running and a next event exists. It lasts at most
  `hold_current_minutes` (default 60; 0 = off) after the event's real end.
- Only the most recent event that ended is held, and **never a maintenance window**: a current maintenance event
  makes the game refuse to join. A maintenance window that just ended holds nothing.
- The held event keeps its card but is sent with no headliners and no `current_headliner`. The hub's "Now
  Playing" list stays empty, and the tiles read "Next Diamond Stage set coming soon!" without the LIVE badge.
- Only the game's schedule polls see the hold. The Main Stage server, admission of stock players and the website
  follow the real times.

`fuserd mainstage list` shows the held event first, as `HELD until <time>`. The control panel's schedule marks it
"Shown as current in FUSER until …" and keeps it in the default view.

```sh
fuserd mainstage add         -config config.json -name <name> -start <time> [-end <time> | -minutes <n>]
                             [-type event|special_event] [-slots <n>] [-turn-minutes <n>] [-image-url <url>] [-allow-stock]
fuserd mainstage maintenance -config config.json -start <time> [-end <time> | -minutes <n>]
fuserd mainstage list        -config config.json [-hours <n>]
fuserd mainstage remove      -config config.json -event <uuid>
fuserd mainstage assign      -config config.json -event <uuid> -player <id | hype username#NNNN> [-slot <n>]
fuserd mainstage unassign    -config config.json -event <uuid> -player <id | hype username#NNNN>
```

Times are RFC 3339. A running fuserd sees changes within a second. `fuserd mainstage` with no arguments prints the
full usage.

## Community song catalogue

fuserd can serve community song paks whose licence permits it. When a player's crate holds a song their game can't
load, their loader looks it up, downloads the pak and `.sig`, checks their SHA-256 hashes and mounts them.

### Adding songs

Run `fuserd catalogue add` on the machine fuserd runs on, or use the control panel's Asset Library:

```sh
fuserd catalogue add -config config.json -pak JoWo_SeinThem_ShpQn_P.pak -licence "local test only" \
    -name "Seinfeld Theme" -artist "Jonathan Wolff" -genre pop -year 1989
fuserd catalogue list -config config.json
```

`-art cover.png` (or `.jpg`, up to 4 MB) adds album art, which store tiles show before the song is installed.
`-update` replaces an entry's files and bumps its version; without `-art`, it keeps the current art.

**What `add` checks:**
- the pak is version 8, with the community mount point `../../../Fuser/Content/` and no encryption;
- it holds exactly one song CelSongSet, `{DLC,Audio}/Songs/<sn>/Meta_<sn>`;
- nothing is outside the song's own folders (`Audio/Songs/<sn>/`, `DLC/Songs/<sn>/`, `UI/AlbumArt/T_<sn>[_small]`);
- the `.sig` is well-formed;
- the shortname is not an official DLC song or listed in `store.reserved_shortnames_file`.

Files go to `store.files_dir/<sn>/<version>/<sn>_P.pak` and `.sig`, recorded in `store.catalogue_file`. A running
fuserd reloads the catalogue within a few seconds.

### The Diamond Store

A catalogue song can be offered in the game's Elder Credit ("Diamond") store:

```sh
fuserd catalogue publish   -config config.json -shortname JoWo_SeinThem_ShpQn -price 100 [-featured] [-end 2027-01-01T00:00:00Z]
fuserd catalogue unpublish -config config.json -shortname JoWo_SeinThem_ShpQn
```

- **Offers.** Without `-end`, an offer never ends. The game refuses to buy an offer whose end date has passed,
  and shows at most 2 featured and 8 standard offers. `unpublish` withdraws an offer; players who bought it keep
  it. Once the control panel's storefront is saved, it supersedes these listings.
- **Featured art.** A featured tile is 8:15 portrait (396 × 741) and stretches its image to fill. So featured art
  that isn't 8:15 (square album art, or an upload of another shape) is centre-cropped to 8:15 on first use and
  served from `/control/media/`. An 8:15 upload is sent unchanged. An external image URL is never fetched, so it's
  sent uncropped. Standard tiles show the square art unchanged.
- **Balance.** Every player's balance is `store.elder_credit_balance`, and purchases don't lower it.
  `players/xp/` reports the balance; the game also requests it after every purchase. With a balance of 0, only free
  offers can be bought.
- **Ownership.** A purchase only records ownership. The player's loader installs the song, because the game
  itself only unlocks content already on disk.

### Endpoints

| Endpoint | For | What it does |
|---|---|---|
| `POST /api/community/resolve/` `{"paths": [...], "shortNames": [...]}` | the loader; needs a token unless `store.resolve_requires_login` is off | Each query gets `{query, kind, found, song}`. `song` carries `shortName`, `version`, `meta_path`, `pak_url`, `sig_url`, `pak_sha256`, `pak_size`, `sig_sha256`, `sig_size`, `licence`, `source`, `song_name` and `art_url` (when the song has art). A path can be the object path (`…/Meta_X.Meta_X`) or the package path |
| `GET /dl/<sn>/<version>/<sn>_P.pak`, `…/<sn>_P.sig` | the loader | The files. Range requests are supported; the ETag is the SHA-256; cacheable for good (a version never changes) |
| `GET /dl/<sn>/<version>/<sn>.bin` | the game's own DLC downloader | `int32 sigLen`, then the `.sig`, then the `.pak` |
| `GET /dl/<sn>/<version>/<sn>_art.png` (or `.jpg`) | store tiles, the loader | The album art added with `-art` |
| `store/songs/get_song/` | the game | The entry as `FStoreItemResponse`, with `pc_asset_download_url` pointing at the `.bin` |
| `store/get_list/` | the store screen | Catalogue tiles, only with `store.list_in_store` |
| `store/elder_credit_store/get_list/` | the Diamond Store tab | Open offers (`uuid`, a Song asset, `price`, dates, `is_owned` for the player, the art as `featured_image_url`) |
| `store/elder_credit_store/purchase/` `{"uuid", "expected_price"}` | the Diamond Store tab | Records the purchase and answers `num_elder_credits_remaining` (the unchanged balance). Refusals: `price_changed`, `not_enough_credits`, `does_not_exist`, `no_longer_available`, `already_owned`, `not_logged_in` |
| `store/elder_credit_store/get_claimed_assets/` | the game at login | The player's purchased songs, as `{asset_name, asset_type: "Song"}` |
| `players/xp/` | the game at login, after a set and after a purchase | `profile.elder_credits.amount` = `store.elder_credit_balance` (when set). Always stores the stats the player's own `players/get/` returns |

**Other behaviour:**
- **Song manifest.** `store/song_manifest/get/` lists catalogue songs only with `store.add_to_song_manifest`.
- **DLC manifest.** With `song_manifest.pak_file` set, the official DLC manifest is served as
  `/static/dlcmanifest/pc.bin` (`uint32 LE sig length | .sig | .pak`) to clients that have none installed.
  Without it, a fresh install's DLC scan fails.
- **Rate limit.** Resolve and download requests are limited per client address (`store.requests_per_minute`).
- **Logging.** Each download, `get_song` and `resolve` lookup is logged under `STORE`, hit or miss, with a tagged
  client address.

**Checking a deployment:** `scripts/store_check.sh <public_url> <shortname> [token]` resolves a song, downloads it,
checks the hashes and splits the `.bin`. It needs `curl` and `python3`.

## Community songs on Google Drive

Community songs are songs whose files stay on their creator's public Google Drive. fuserd stores only where the
files are, in the `community_songs` table of the database. When `community/resolve/` misses the catalogue for a
song the table knows, the answer keeps `found: false` and adds a `community` link. A loader that understands it
downloads the files from Drive, extracts them, checks the pak layout and installs it. Other loaders ignore it and
ask the other players in the session instead (see [song transfers](#song-transfers-between-players)).

```sh
fuserd community add     -config config.json -x <X> -kind archive -archive zip|rar4|rar5 -file <id>,<size>,<name> \
                         -pak-entry <path in archive> -sig-entry <path> -link <url> [-artist] [-title] [-creator] [-note]
fuserd community add     -config config.json -kind folder -x <X> -file <pak id>,<size>,<X>_P.pak -file <sig id>,<size>,<X>_P.sig -link <url>
fuserd community import  -config config.json -batch batch.jsonl [-update]
fuserd community update  -config config.json -x <X> [any add flag]     # only the flags given change
fuserd community disable -config config.json -x <X> [-reason "creator asked"]
fuserd community enable | remove | show -config config.json -x <X>
fuserd community list    -config config.json [-creator <text>]
```

- **Batch import.** `import` reads one JSON entry per line, the same object `add -json` takes. Blank lines and
  lines starting with `#` are skipped. It adds new entries and, with `-update`, replaces changed ones.
- **Refused:** official shortnames (the DLC manifest's songs and `store.reserved_shortnames_file`), creators
  containing "Harmonix", unsafe archive paths, and files over 128 MB.
- **Changes apply at once,** because a running fuserd reads the table on each resolve.
- **The catalogue answers first:** a shortname in both is served from the catalogue.

**Reports.** The loader sends every outcome to `POST /api/community/report/`
`{x, ok, reason, detail, sha256, sig_sha256, size, ms}`.
- `reason` is one of `http`, `quota`, `html`, `size`, `extract`, `layout`, `sig` or `install`.
- A good install records its pak's SHA-256. Resolve lists those hashes as `seen_sha256`, for information only.
- When two different players report a bad file (`layout` or `sig`), the entry is disabled. `show` lists the
  reports.
- An `update` that changes the entry, or `enable`, clears the failure reports.

**Logging (`STORE`):** `resolve … community=<n>` for each resolve request, `community resolve x=…` for each
community answer, `community report x=… ok=… reason=…` for each report, and `community entry disabled` when
reports disable an entry.

## Peer-to-peer connectivity

With `p2p.enabled`, players' loaders can connect to each other directly, without port forwarding. fuserd provides
a STUN service, signaling, an optional TURN relay, and song transfers between players.

```json
{
  "p2p": {
    "enabled": true,
    "stun_port": 3478,
    "stun_alt_port": 3479,
    "stun_host": "",
    "stun_requests_per_second": 20,
    "requests_per_minute": 300
  }
}
```

### STUN

The loader's tunnel socket learns its public UDP endpoint from fuserd's STUN service.

- **What it answers:** STUN Binding requests (RFC 8489). Anything else gets no answer.
- **The second port** (`stun_alt_port`, same address) lets a client tell whether its NAT gives a different public
  port per destination. Such a NAT is "symmetric", and hole punching through it is unlikely to work. `0` turns it
  off.
- **Firewall:** open UDP `stun_port` and `stun_alt_port` inbound.
- **Clients** get the endpoints from `GET /api/p2p/config/` → `{"enabled", "stun": [{"host","port"}, ...],
  "relay"}`, primary first. `stun_host` defaults to `public_url`'s host.
- **Limits:** requests over `stun_requests_per_second` per client IP are dropped.
- **Logging:** one `P2P` line per STUN port and client endpoint per minute.

To check the service from a player's network:

```sh
fuserd stun-probe <server-host>          # ports 3478 and 3479
fuserd stun-probe -alt-port 0 host:3478   # one port only
```

It prints the local socket, the public endpoint each port saw, and the NAT mapping:
- `none`: a public address;
- `endpoint-independent`: hole punching should work;
- `endpoint-dependent`: symmetric NAT, a relay is needed.

### Signaling

A player-hosted session's loaders swap their tunnel sockets' endpoints through fuserd. Every endpoint needs a
login. Roles come from matchmaking: the host is the session's creator, and a joiner is any other member.

| Endpoint | Who | Body → answer |
|---|---|---|
| `POST /api/p2p/host/` | the host | `{game_session_id, candidates, mapping}` → `{role:"host", peers:[{peer_id, key, candidates, mapping}], poll_ms}` |
| `POST /api/p2p/join/` | a joiner | the same body → `{role:"joiner", peer_id, key, host:{candidates, mapping} \| null, relay, poll_ms}` |
| `POST /api/p2p/poll/` | either | `{game_session_id}` → that side's answer, refreshing its registration |
| `POST /api/p2p/report/` | either | `{game_session_id, peer_id, state, path, rtt_ms}` → `{}`, logged |

- **`candidates`:** 1–8 `{type: "host"|"srflx"|"relay", ip, port}`. A joiner adds its TURN allocation as `"relay"`.
- **`peer_id`** is random, non-zero and unique in the session. **`key`** is 32 random bytes in hex, for the pair's
  tunnel packets. Both stay the same for a joiner across calls.
- **Lifetimes.** A joiner may register before the host; `host` stays `null` until the host does. Registrations
  expire two minutes after their last call. Players who leave the session drop out of the host's list, and an
  ended session is forgotten.
- **Privacy.** A player's addresses are given only to the other side of their own session, and aren't logged.
- **Limits.** Each player may make `p2p.requests_per_minute` (300) host, join, poll and report calls a minute, and
  each address as many `p2p/config/` calls. Over that, the answer is HTTP 429. A host polls once a second, so keep
  the limit well above 60; `0` turns it off.
- **Logging:** `P2P` lines for `host registered`, `joiner registered` and `connection report`.

### TURN relay

With `p2p.turn_enabled`, fuserd also runs a TURN relay for player pairs whose NATs can't be punched, typically a
phone carrier's symmetric NAT.

- **How it's used.** Joiners get credentials in the `relay` field of `p2p/join/` and `p2p/poll/`. The loader
  allocates a relay address and registers it as a `relay` candidate; the host just sends to it.
- **Credentials** are TURN REST credentials: username `<expiry>:<player id>`, password
  base64(HMAC-SHA1(`turn_secret`, username)). An empty `turn_secret` is derived from `token_secret`. They last
  `turn_credential_minutes` (12 h by default). An allocation keeps its username, so this also bounds a relayed
  session.
- **Limits.** `turn_max_per_player` concurrent allocations, and `turn_bytes_per_second` per allocation and
  direction (64 KB/s by default). Game traffic is about 0.4 KB/s per player.
- **Firewall:** open UDP `turn_port` and UDP `turn_relay_port_min`–`turn_relay_port_max` inbound.
  `turn_relay_address` must be the server's public IPv4 address.
- **Logging:** `P2P` lines `relay allocated`, `relay permission`, `relay released`, `relay auth refused` and
  `relay quota reached`.

### Song transfers between players

With `p2p.enabled` and `p2p.song_transfers` (default on), a player's loader can ask other members of its session
for a custom song their crates hold. This works in any session, player-hosted or on a public server. fuserd only
arranges the transfer: the two loaders punch a path and move the file directly, so no file passes through fuserd.
There's no relay for transfers (the `relay` field is reserved). Every endpoint needs a login and session
membership, and counts against `p2p.requests_per_minute`.

| Endpoint | Who | Body → answer |
|---|---|---|
| `POST /api/p2p/song/request/` | requester | `{game_session_id, song_path, owners}` → `{transfer}`. `song_path` must be `/Game/DLC/Songs/<X>/Meta_<X>.Meta_<X>`. `owners` are the members whose crates hold it. An open request for the same song is returned as is |
| `POST /api/p2p/song/poll/` | every member, every 2 s | `{game_session_id, sending, syncing, sync_failed, songs, missing}` → `{transfers, poll_ms, sync, conflicts}`. `transfers` are the player's transfers, as requester or as the owner currently asked. `sending: false` declines every request waiting on the player. `syncing` / `sync_failed`: whether this player is fetching custom songs now, and how many of its batch it couldn't get. `songs` / `missing`: the song report (below) |
| `POST /api/p2p/song/offer/` | the asked owner | `{game_session_id, transfer_id, size, sha256, sig_size, sig_sha256, candidates, mapping}` → `{transfer}` with the pair id and key |
| `POST /api/p2p/song/decline/` | the asked owner | `{game_session_id, transfer_id, reason}` (`not_found`, `not_custom`, `disabled`, `busy`) → `{transfer}`, now asking the next owner |
| `POST /api/p2p/song/accept/` | requester | `{game_session_id, transfer_id, candidates, mapping}` → `{transfer}`. May be sent again with new candidates after a network change |
| `POST /api/p2p/song/report/` | either side | `{game_session_id, transfer_id, state, reason, bytes, ms, path}` → `{transfer}`. Either side may report `transferring`. Only the requester reports `done` or `cancel`. Any other state moves on to the next owner: from the requester `no_path`, `stalled`, `rejected` or `failed`; from the owner `failed` |

**A transfer, as each side sees it:** `transfer_id`, `song_path`, `song` (`<X>`), `state`, `reason`, `role`
(`requester` or `owner`), `requester`, `owner`, `attempt`, `pair_id`, `key`, `file` (`{size, sha256, sig_size,
sig_sha256}`), `peer` (the other side's `{candidates, mapping}`) and `relay` (always null).
- `pair_id`, `key` and `file` appear once the current owner has offered. The owner sees `peer` once the requester
  has accepted.
- A transfer that drops out of a player's poll is over for that player.

**States:** `asked` → `offered` → `connecting` → `transferring` → `done`. `failed` means no owner was left;
`cancelled` means the requester left or withdrew, or the session ended.

**Moving on to the next owner** happens when:
- the current owner declines, doesn't answer within 15 s, or leaves;
- the requester doesn't accept within 15 s;
- no path comes up within 20 s;
- an attempt runs past 5 minutes;
- a failure is reported.

Owners are asked in order: polling senders first, then the less busy, then players who haven't polled. Players
known not to send are skipped. A request may name up to 8 owners, and a requester may have up to 8 open requests.
Finished transfers stay visible for a minute.

**Sync rounds.** A round starts when any player polling the session reports `syncing`, and finishes when none
does. The poll's `sync` field is `{round, active, done_round, syncing, players, failed}`: `players` and `failed`
are the finished round's players who fetched songs and the songs they couldn't get. Each loader announces a
finished round to its player. A player who stops polling for 10 s no longer holds a round open.

**Song versions.** A loader's poll may carry a song report: `songs`, `[{song, sha256}]`, the custom songs in the
session's crates that its machine holds, with each pak's sha256, and `missing`, the crate songs its fetches ended
without. Each report replaces the player's last one; a poll without `songs` (an older loader) keeps it. The answer's
`conflicts`, `[{song, versions, players}]`, are the songs the session's members hold as different files (shortnames
compared without case; official and catalogue songs left out). Players with different files hear different stems
or miss cels, so each loader tells its player about every conflict. The reports also feed the
[held-back custom songs](#fuserdedicatedserver-fds) of an FDS session.

**Logging:** `P2P` lines for `song requested`, `song offered`, `song accepted`, `song report` and
`song transfer next owner|failed|cancelled`, plus `song sync round started` / `finished` and (once per song and
session, as a warning) `custom song held as different files`. Song polls log as `POLL`.

## Profile pictures

The game renders a player's profile picture from their avatar as three JPEGs (large, medium and small; about
150–190 KB for the large one) and uploads them whenever the avatar changes. Other players' games show the picture
on player cards, in lists and on profiles. Players who haven't uploaded one show the default image.

| Step | Request | fuserd |
|---|---|---|
| Upload URLs | `players/profile_pic/` (logged in) | Three one-time URLs, `<public_url>/pic/upload/<token>`, valid for `profile_pics.upload_minutes` |
| Upload | `PUT` (or `POST`) `/pic/upload/<token>`, the JPEG as the body, no login | Checks it's a JPEG or PNG of at most `max_kb` and 2048 × 2048, and stores `<dir>/<player>/<pic id>_<size>.jpg`. A newer picture replaces the player's previous one, whose files are deleted |
| Show | every user record (`players/get/`, search, followers, invites) | `profile_pic {pic_id, asset_download_url, asset_medium_download_url, asset_small_download_url}`. Each upload gets a new `pic_id`, so games fetch the new picture |
| Download | `GET /pic/<player>/<pic id>/<size>.jpg` | The image, cacheable. A size not uploaded is served from the nearest one. Rate-limited per address like `/dl/` (`store.requests_per_minute`) |

**Configuration** (`profile_pics`): `enabled` (default `true`; when off, the game gets no upload URLs and everyone
shows the default image), `dir` (default `data/profile_pics`), `max_kb` (default 512) and `upload_minutes`
(default 10).

**Moderation.** `fuserd profile-pic remove -config config.json -player <id>` removes a player's picture record and
files. Every game shows the default image again until the player's game uploads a new one. The player id is
fuserd's own (`user_id` in profile responses).

## Administrator control panel

The panel at `/control/` is disabled by default. Enable it in the configuration:

```json
"control": { "enabled": true, "origin": "https://fuser.example.org", "allowed_networks": ["203.0.113.7/32", "2001:db8:42::/48"], "timezone": "Europe/London" }
```

- **`origin`** is the exact browser origin: scheme, hostname and optional port, with no trailing slash or path.
  Use HTTPS with a hostname in production. For local development, use `http://localhost:8000` and open that
  hostname; WebAuthn relying-party IDs can't be IP addresses. Set `public_url` to the reachable origin used for
  game artwork.
- **`allowed_networks`** lists the CIDR networks whose addresses may use the panel. An empty list (the default)
  allows every address. Use `/32` for a single IPv4 address and `/128` for a single IPv6 address; an address
  without a prefix length stops startup. Any other address gets `403` on every `/control/` route before anything
  else is checked. Published artwork (`GET /control/media/...`) stays public, because FUSER downloads it. If your
  address changes and locks you out, edit the list and restart; the `fuserd control` commands work locally
  regardless.
- **`timezone`** is the fallback IANA zone for panel times; see Timezones below.

The panel expects fuserd to terminate TLS itself (`tls_cert` / `tls_key` or `acme`). Rate limits, `allowed_networks` and the
audit trail use the connection's peer address and never read `X-Forwarded-For`, so behind a reverse proxy they'd
see only the proxy. With an HTTPS origin, control responses served over TLS carry
`Strict-Transport-Security: max-age=31536000` for this hostname only (no `includeSubDomains`, no preload).

### Administrators

Create an expiring enrollment token on the server, with the same configuration and working directory as the
running service:

```sh
fuserd control enroll -config config.json -name "Operator" -expires 15m
```

The command prints a random 16-character hexadecimal token once, and stores only its SHA-256 hash. Expiry defaults
to 15 minutes, with a 24-hour maximum. Enter the token on `/control/`, then enroll a discoverable WebAuthn passkey
with user verification. Storing the credential consumes the token; a failed or cancelled ceremony doesn't. Later
sign-ins accept only WebAuthn assertions: there are no passwords or other login shortcuts.

```sh
fuserd control list -config config.json                      # administrators and unused enrollment tokens, with their IDs
fuserd control revoke -config config.json -id ADMIN_ID       # remove an administrator
fuserd control revoke-token -config config.json -id TOKEN_ID # cancel an unused enrollment token
```

Revoking an administrator ends their sessions immediately and cancels any enrollment tokens they issued.

The panel's **Administrators** tab does the same from the browser:
- *Invite administrator* creates an enrollment token for a named person, valid for up to 60 minutes. The token is
  shown once, with a copy button, and only its hash is stored. Send it over a private channel.
- The tab lists administrators (enrolled, invited by, last sign-in), pending invitations (which can be cancelled)
  and the newest 200 audit entries.
- An administrator can remove any other administrator but not themselves, so the panel can never remove the last
  one. Use `fuserd control revoke` for that.

### Security

- **Storage.** Admin credentials and eight-hour sessions are stored separately from game accounts in the SQLite
  database. Back up the database to keep credentials, panel settings, notes, uploaded images and the audit trail.
- **Cookies** are HttpOnly and SameSite=Strict. With an HTTPS origin they're `__Host-fuser_control` and
  `__Host-fuser_ceremony` (Secure, `Path=/`, no Domain), so neither a sibling subdomain nor a plain-HTTP response
  can plant them. HTTP localhost development uses unprefixed names.
- **Ceremonies.** Challenges expire after three minutes and are single-use. Mutations require the configured
  Origin. Enrollment and login requests bypass body logging, so tokens and WebAuthn responses are never logged.
- **Rate limits.** Authentication is limited to 20 requests a minute per IPv4 address or per IPv6 /64, and a
  client keeps at most three pending ceremonies. Both tables are bounded, and when one is full the oldest entry is
  dropped instead of refusing new clients. A flood from many addresses slows sign-in down but can't lock
  administrators out.
- **Public routes.** Only the sign-in page, embedded scripts, ceremony endpoints and published artwork at
  `/control/media/{sha256}` are public. Everything else needs an admin session.
- **Audit trail.** Every sign-in, enrollment, sign-out, invitation, removal and panel change is stored in the
  `control_audit` table with the administrator, client address, action and target, and written to the always-on
  `CONTROL` log category. In the log, client addresses, player IDs and Epic keys are tags; the table keeps the
  values. Command-line actions are recorded as *Command line*. A passkey whose signature counter goes backwards (a
  possible clone) is refused and recorded as `auth.clone_warning`. Failed and rate-limited authentication attempts
  are logged but not stored, so unauthenticated clients can't grow the table.

### Timezones

At sign-in the browser reports its IANA timezone, and the session shows and enters every panel time in that zone.
The active zone and its current UTC offset are shown on the pages. If the browser reports none, or one the server
can't load, the session uses `control.timezone`. When that's empty, it's UTC; an unknown name (or `Local`) stops
startup. The zone is fixed for the session, so sign in again after travelling. The timezone database is compiled
into fuserd, so names resolve on Windows and minimal hosts too.

Stored times are RFC 3339 UTC and don't depend on any of this. Control documents with wall times and no offset are
read as UTC+08:00.

### Tabs

- **Server status** (the tab the panel opens on) shows what the landing page shows (players online and every public FDS lobby with its venue,
  state, set length and fill), plus the private lobbies. Each lobby lists its members: name, user ID, performer or
  viewer, the creator, and when they joined. Members are players in the lobby seen online in the last minute.
  Lobbies whose server is still starting show as *Server not ready*. The tab refreshes every 10 seconds while it's
  open. The Main Stage and player-hosted sessions aren't listed.
- **Mainstage schedule** lists stored events and the next seven days of rotation events. Editing a rotation event
  stores an override, and removing one suppresses it. Headliner edits refuse to discard bookings or slot blocks
  changed since the editor opened. Maintenance replaces overlapping rotation events.
- **Diamond Store** edits the eight store slots (2 featured, 6 standard). Once the storefront is saved, its prices,
  ordering, enabled flags and dates supersede the catalogue's Elder Credit listings for display and purchases,
  without removing players' ownership.
- **Community songs** manages the [community song table](#community-songs-on-google-drive), as
  `fuserd community` does, with the same rules: official shortnames and rows credited to Harmonix are refused.
  - The list is paged on the server (25 to 250 rows), so it stays quick with the >12,000-entry sheet. Search
    matches the shortname, artist, title or creator; filters pick enabled, disabled or failing entries (any failed
    loader report) and archives or folders; sorting is by last update, artist, creator or shortname. The counts
    above the list cover the whole table.
  - Edit opens an entry with its checks, latest loader reports and good pak hashes; Disable (with a reason), Enable
    and Remove are there too. Saving a changed entry re-enables it and clears its reports, because its files are
    new. The shortname can't change: remove the entry and add it again.
  - **Bulk import** takes a `.jsonl` file (the `fuserd community import` format) or a `.json` file (one entry or an
    array) of up to 32 MB. Choosing the file runs a dry run listing each line as added, updated, unchanged, kept
    or refused; *Apply* uploads it again and stores the added and updated entries in one transaction, all or
    nothing. *Add new and update changed entries* is the CLI's `-update`. Refused lines are skipped.
  - **Export .jsonl** downloads every entry in the import format; disabled state and reports aren't included.
  - Every change is in the audit trail (`community.add`, `.update`, `.disable`, `.enable`, `.remove`, `.import`).
- **Promotions** edits the main-menu carousel (up to 8 promotions), served by
  `main_menu/promotions/get_active/`: every enabled promotion inside its start/end window, in carousel order. FUSER
  shows one tile per promotion, cycled with W/E (LB/RB) and automatically every 5 seconds. fuserd assigns each
  `promotion_id` and never reuses one: a new promotion, or one whose image changes, gets the next id, because a
  running FUSER caches a promotion's image by its id. The browser preview approximates the game UI.
- **Asset Library** imports community song paks, either with a separate `.sig` or as a ZIP containing one pak and
  its signature.
  - Name, artist and licence/sharing permission are required; source and artwork are optional. Shortnames are
    detected from the package.
  - Imports use the catalogue importer: reserved songs and duplicate assets are refused, and `store.max_pak_mb`
    applies (up to 2048 MB). ZIP paths and expanded sizes are checked before import. It needs
    `store.catalogue_file` and `store.files_dir`.
  - Uploaded assets appear in the store's offer editor straight away, but aren't offered until selected and saved.
  - Removing an unused asset archives it; its catalogue entry and files stay available to existing owners.
    Linked offers must be cleared or changed first.
  - Command-line catalogue changes and panel imports share a `<catalogue_file>.lock` directory. After a writer
    crashes, remove a stale lock only once you've confirmed that writer has stopped.
- **Users** shows players, their details and presence (from recent game pings, refreshed every 30 seconds when no
  editor is open). Bans reject existing game sessions as well as new logins.
- **User exemptions** manages [Epic emulator exemptions](#epic-emulator-exemptions). It still requires
  `auth.allow_epic_failed_signatures`, and verified or blocked Epic identities can't be turned into exemptions.
- **Administrators**: see [Administrators](#administrators).

**Uploaded images** (PNG, JPEG, WebP or GIF up to 3 MB) have their dimensions validated and their first frame
re-encoded as 8-bit PNG. FUSER renders a 16-bit image as an overbright texture, so every image is served as 8-bit.

## Logging

Every line carries a category, `cat=`.

- **Categories.** API routes log under their module, upper-cased: `AUTH`, `STORE`, `PLAYERS`, `MULTIPLAYER`,
  `MAIN_MENU`, and so on. The exceptions are:
  - `POLL`: the endpoints the client calls every few seconds (`players/ping/`, `feed/pull_notifications/`, `nop/`,
    `p2p/song/poll/` and the `mainstage/` schedule URLs);
  - `HTTP`: unknown paths;
  - `SERVER`: startup and shutdown;
  - `CONTROL`: the administrator panel's audit trail.

  The full list is in `internal/logx`.
- **Choosing output.** `log_categories` picks what is output, e.g. `["AUTH", "STORE"]`; `["*"]` or an empty list
  means everything. `log_disabled_categories` then switches categories off again, e.g. `["POLL"]` for everything
  but the clients' polling (the example config does this). Names are case-insensitive, and a misspelt one stops
  startup. `SERVER` and `CONTROL` lines and anything at Error level are always output. The `listening` line at
  startup lists the active categories.
- **Tags, not identities.** Player IDs, platform user IDs, usernames and tokens never appear verbatim. Each is
  replaced by a tag: the first 12 hex characters of HMAC-SHA-256 over the value. The key is `log_tag_secret`, or
  one derived from `token_secret` when that's empty.
  - A tag is stable for as long as its key, so one player's lines can be grepped together.
  - Without the key, a tag can't be reversed by hashing guessed SteamIDs.
  - To find a player's lines, run `fuserd -config config.json -tag <value>` with their SteamID, Epic account ID or
    player ID. It prints the tag. Grep for `platform_user=<tag>` on the `login` line to get their `player=` tag.
- **`log_file`** (e.g. `"fuserd.log"`) also appends every line to that file, identical to the console output.
  Relative paths are from the working directory. Empty means stderr only. fuserd never rotates or truncates the
  file.
- **`log_bodies: true`** logs request and response bodies for enabled categories. The values of identifying keys
  (`token`, `user_id`, `platform_user_id`, `username`, `friends_list`, ...; the list is in `internal/logx`) are
  tagged, and non-JSON bodies are logged only by size. Numeric IDs get the same tag in a body as on the request
  line.

## Command reference

| Command | Purpose |
|---|---|
| `fuserd -config config.json` | Run the server |
| `fuserd -config config.json -tag <value>` | Print the log tag of a value and exit |
| `fuserd catalogue add\|publish\|unpublish\|list` | Manage the community song catalogue and Diamond Store offers |
| `fuserd community add\|import\|update\|disable\|enable\|remove\|show\|list` | Manage Google Drive community song links |
| `fuserd mainstage add\|maintenance\|remove\|list\|assign\|unassign` | Manage the Main Stage schedule and headliners |
| `fuserd control enroll\|list\|revoke\|revoke-token` | Manage control panel administrators |
| `fuserd exempt` | Provision an Epic emulator exemption |
| `fuserd profile-pic remove` | Remove a player's profile picture |
| `fuserd stun-probe` | Check the STUN service from a player's network |

Each command prints its usage when run without arguments.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/fuserd` | Entry point, HTTP server wiring and the command-line tools |
| `internal/api` | Response envelope, route table (every endpoint), handlers, the landing page and the control panel's assets (`site/`) |
| `internal/catalog` | Community song catalogue: pak inspection, the `catalogue add` builder, lookups |
| `internal/community` | Google Drive community song entries and their checks |
| `internal/config` | Configuration file |
| `internal/control` | Control panel authentication (WebAuthn), sessions and audit |
| `internal/fdstoken` | Signed player session tokens for FDS containers |
| `internal/logx` | Categorised logging and pseudonymous tags |
| `internal/mainstage` | Main Stage schedule rules |
| `internal/matchmaking` | In-memory game sessions, tickets, player sessions and invites |
| `internal/p2p` | Peer-to-peer signaling |
| `internal/platform` | Steam ticket and JWKS/JWT verification |
| `internal/publichost` | Docker-managed FDS game servers |
| `internal/songxfer` | Song transfer signaling between players |
| `internal/store` | SQLite storage: players, Steam DLC ownership, social graph, blocks, stats, profile pictures, purchases, community songs, Main Stage bookings, control panel data |
| `internal/stunserver` | STUN service and the `stun-probe` client |
| `internal/token` | Session tokens issued by `auth/jwt/` |
| `internal/turnrelay` | TURN relay |
| `scripts/store_check.sh` | End-to-end check of the catalogue endpoints on a deployment |
| `data/dlc_songs.json` | The 133 official DLC song shortnames for `store/song_manifest/get/` (names only, no content) |
| `data/dlcmanifest.pak`, `.sig` | The official DLC manifest (`song_manifest.pak_file`), unchanged from an install's `PersistentDownloadDir/DLC/Manifest/` |
| `config.example.json` | Example configuration with every setting |
| `Dockerfile` | Linux build that exports `build/fuserd` |
| `fuserd.service` | systemd unit |
