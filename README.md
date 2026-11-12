# DOCUMENTATION TBC


# q2-jellyfin

A [Jellyfin](https://jellyfin.org) client for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Pick playlists, albums, artists and songs on the Q2, and it syncs them to the card over Wi-Fi, or streams a song straight from your server. It needs a Q2 Pod build that supports card apps, and a Jellyfin server reachable over HTTPS. Its requests are checked against the Jellyfin 12.4.0 API document.

> **Status: first release (0.7).** Its tests run against a real Jellyfin 12.4.0 server, and test builds have run on a Q2 with Q2 Pod 1.8.0. A few features (the one-page sign-in, ⋯ showing the play queue, the heart, and reporting every streamed song) use Q2 Pod card-app features that are still pull requests; on firmware without them the app keeps its older pages and behaviour. Forked from [q2-subsonic](https://github.com/andsuor/q2-subsonic).

## Install

**Over Wi-Fi:** once listed in Q2 Pod's App Store (`sources.list`), open **System settings → App Store** and press **Jellyfin**.

**By hand:**

1. Download `q2-jellyfin-*.zip` from the latest release.
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.jellyfin/q2-jellyfin`, `.jellyfin/run`, `.jellyfin/q2app` and its icons `.jellyfin/icon.png` and `.jellyfin/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

**Jellyfin** then has a row in **Streaming**.

## Sign in

Everything is done on the Q2 itself. The app opens no network port and serves no web page, so nothing it does crosses your Wi-Fi except HTTPS to your server.

**First, the server's address.** Open **Streaming → Jellyfin** and press **Type Your Server Address**. Type it on the Q2's keyboard, for example `music.example.com` or `music.example.com:8920` (`https://` is added for you). The Q2 checks it is a Jellyfin server before keeping it. **Change Server** at the top of the sign-in list opens the address: press it to type a new one. On Q2 Pod firmware with `field` (in `/tmp/q2-addon.caps`) the address is a **Server Address:** row instead: press it and type; an address that doesn't work says why, with **Retry** and **Cancel**.

**Then sign in on the Q2, in this order of preference:**

1. **Sign In with Quick Connect** (recommended). The Q2 shows a six-digit code. In any Jellyfin app where you're already signed in (the web page, a phone app), open **User Menu → Quick Connect** and enter it. The Q2 signs in on its own. Your password never reaches the Q2. Your server's admin must have Quick Connect turned on (**Dashboard → General**).
2. **Sign In with Password.** Type your user name, then your password, on the Q2's own keyboard. The password goes only from the Q2 to your server over HTTPS, is swapped there for an access token, and is never stored. Q2 Pod's keyboard has no hidden-text mode, so the password shows on screen while you type it; once typed it is never shown (the page says *Login Success*). A wrong password is sent once: Retry says why again, and Return asks for another. A password the same as the user name can't be typed this way (the Q2 takes it for the user name): use Quick Connect.

   On Q2 Pod firmware with `field` (in `/tmp/q2-addon.caps`), it is one page instead: press **Username:** and type your user name, which then shows beneath it; press **Password:** and type your password, which then shows as `***`; then press **Sign In**. A sign-in that fails (a wrong password, the server out of reach) says why, with **Retry**, which brings the page back with the password cleared, and **Cancel**. The password is tried once and then forgotten, so it is never sent twice: type it again to try again. What you typed is kept only in the app's memory, never on the card, and is forgotten once you sign in or go back (to the sign-in list, or to Account). With no Wi-Fi, Sign In sends nothing and the password stays for when Wi-Fi is back. On this page a password may be the same as the user name. **Settings → Account → Signed in as** opens the same page, to sign in as another user; the sign-in you have stays until the new one works.

Sign-in with a Jellyfin API key is turned off for now: a Jellyfin API key is **admin-level** (it belongs to no user and opens every library on the server), so the Q2 doesn't offer it. A Q2 already signed in with one stays signed in.

Things to know:

- The server must be `https://`; `http://` is refused. Certificates are verified, and the token is never sent to another host on a redirect.
- **Only an access token is stored on the card**, in `.jellyfin/credentials`: the token from Quick Connect or a password sign-in, never a password. It is encrypted with a key kept on the Q2 itself, so the card on its own doesn't give it away, but whoever has both can use it. **Tip: use a Jellyfin user just for the Q2, not an admin**, so a lost Q2 opens only that user's music. If it is lost, revoke the Q2 under **Dashboard → Devices**. A Q2 signed in with an API key before this version keeps that key on the card instead; if that card or Q2 is lost, delete the key under **Dashboard → API Keys** (revoking the device doesn't cancel an API key).
- **Settings → Account** shows the server and who is signed in; press **Signed in as** to sign in as another user with a password; **Sign Out** removes the token and shows the sign-in. Your music stays on the card.
- Syncing in **Original** quality needs the user's **Allow media downloading** permission on the server; Opus transcodes don't.

On Q2 Pod firmware that has them, Jellyfin's Loading… and error pages have no **Stop Jellyfin** row (an error page has **Retry** and **Cancel**, Cancel going back as Return does), and **⋯** on Now Playing, while Jellyfin's songs play, opens the **Play Queue** instead of the Q2's own ⋯ page: the songs queued to play (up to 100 before the one you pressed and 400 from it on). It opens at the song playing, with the songs after it following, then *Earlier songs*. The song playing is marked *Now Playing* once the Q2 says it plays (a Q2 Pod with `now`); until then, and on an older Q2 Pod, the song you pressed is marked *Started here*. Press a song to play the queue from it. It needs no Wi-Fi to show; after the Q2 is switched off, or after Sign Out, it says *Nothing from Jellyfin is playing* until you play something again. Older firmware ignores both.

## Use

**Music only.** A Jellyfin server can also hold films, shows, audiobooks, photos and mixed-content folders. q2-jellyfin only ever lists, searches, syncs and streams songs from the server's **music libraries** (libraries of the Music type): no film, audiobook or file from another kind of library appears, even when it is a favourite or sits in a playlist. To make something available on the Q2, put it in a Music library on the server.

The app opens on its sections: **Newly Added** (the latest albums), **Albums**, **Artists**, **Playlists** and **Settings**. Albums, Artists and Playlists each have a search box at the top that searches that section (Albums finds albums and songs). **Settings** has **Sync** at the top (its subtitle on the main page shows how the sync stands), then **Quality and Storage**, **Swipe Between Sections**, **Account** and the version. On Q2 Pod firmware that has `sections` (in `/tmp/q2-addon.caps`), each section is a page of its own instead, and a sideways swipe moves to the next or previous one. **Settings → Swipe Between Sections** turns this off and on (on by default; it applies when Jellyfin is next opened). On firmware without `sections` the row says it is unavailable; a card that was swiping and is moved to such firmware turns the setting off, and it stays off until it is turned on again. If a stream fails, the message shows at the top of Settings. On older firmware the sections are rows of the main page.

**Favourites**, at the top of **Playlists**, lists the songs you've marked with the heart in Jellyfin, like Liked Songs or Tidal's My Collection. Play it, or add it to Sync like a playlist: its songs are synced and a **Favourites** playlist appears on the Q2, following your hearts at each sync.

**Choosing what to sync:** hold **Play/Pause** on a playlist, album, artist or song to toggle it in Sync, or open it and press **Add to Sync** / **Remove from Sync**.

**Quality.** By default every song is the server's own file, untouched: **Original (no transcoding)**. To save space or data you can choose an **Opus** transcode instead, at 320, 288, 256, 224, 192, 160, 128, 96 or 64 kbps; the Jellyfin server does the converting, and the Q2 receives Opus files. Both qualities are set in **Settings → Quality and Storage**: **Sync Quality** is every synced song's, and **Streaming Quality** is what you stream. Each opens a list of the choices, Original first, then Opus from 320 kbps down, with the current one marked **Selected**. When you change Sync Quality, the next sync brings the songs already on the Q2 again at the new quality (the preview shows how much).

Two exceptions are automatic: an original the Q2 can't play (WavPack, Musepack, …) arrives as Opus 320, and on Q2 Pod firmware without the `playlist` extension, whose folder playback plays only FLAC and MP3, a streamed transcode is MP3 at the chosen bitrate.

**Syncing:** open **Settings → Sync**. Its options are always at the top of the page, whatever the sync is doing:

- **Sync Jobs**: what is in Sync and not yet on the Q2; press one to hold it or resume it.
- **On This Q2**: everything chosen, with its status and its size on the card.
- **Sync Favourites**: one press adds your favourites (the songs you've hearted in Jellyfin) to Sync and starts the sync. Once they're in Sync, the same press syncs again, picking up new hearts.
- **Choose Playlists**: your playlists, each marked **In Sync** or not; press one to add it or take it out. Each comes to the Q2 as a playlist, in the server's order, duplicates included.
- **Sync Entire Library**: press to sync every album and playlist (On, with the counts once checked), or press again to stop.

Everything synced is at **Sync Quality** (Original, or the Opus rate you chose).

Below them, Sync shows a preview, and nothing happens until you press **Start Sync**. Then it shows progress with **Pause**, **Resume** and **Cancel**. If Wi-Fi drops or the Q2 restarts, it picks up where it was. Songs that fail are listed under **Failed Songs**, with **Retry Failed**.

**Playing:** press a song to play it and the rest of the list after it. Songs already synced or in the playback cache play from the card at once. When the song has to arrive first, a *Buffering…* message shows and the song's row says what is happening (*Connecting…*, *Buffering 40 %*, *Slow connection · buffering 20 %* or *Downloading 60 %*) until Now Playing opens. Once it plays, the list is an ordinary list again, and Return from Now Playing comes back to it. The others stream:

- A FLAC (or WAV) original starts after about 3 seconds when the connection is at least twice as fast as the song, and otherwise after at least 15 seconds and enough that the rest arrives in time even if the connection slows to 60 %. The speed is measured over the whole download and over the last 3 seconds, and the slower counts.
- Other formats, and every Opus transcode, play once they have arrived whole (a transcode is small). Their files don't say the song's whole length until they are complete, so starting them early would confuse Now Playing and seeking.
- The next two songs download in the background in the same way, so they are queued, and **Next** goes to them, as soon as they can start. Next can only go to a song once its file is on the Q2 (Q2 Pod queues a card app's songs as they arrive), so on a slow connection it may take a few seconds after the song starts before Next works.

Q2 Pod's player can't tell the end of what has arrived from the end of the song, and a card app can only give it files. So skipping ahead in a song that is still downloading, past the part that has arrived, ends the song, and a connection that drops sharply mid-song can still cut one short. A card app can't show anything over Now Playing, so there is no buffering message mid-song.

**Lyrics:** when your Jellyfin server has a song's lyrics (a .lrc file beside it on the server, or lyrics in its tags), they come with the song, synced or streamed, and show on Now Playing, lit line by line when they're timed. They only ever come from your server.

**Playback cache:** like Q2 Pod's Tidal cache, a song you stream is kept on the card once played, and plays from there, without Wi-Fi, the next time you play it at the same quality. **Settings → Quality and Storage → Playback Cache** sets its size: Off, 512 MB, 1 GB, 2 GB (the default), 4 GB or 8 GB; the songs played longest ago go first. **Clear Playback Cache** (press twice), below it, empties it. The cache is separate from Sync: cached songs don't show in the Q2's library, and a sync that needs their space takes it. A sync that brings a song already in the cache at Sync Quality moves it out of the cache instead of downloading it again.

**Reporting plays:** your Jellyfin server counts what you play on the Q2, as it does for its own apps: the song's play count, *Played* mark and last-played date, which the server's lists sort and filter by. **Settings → Report Plays to Server** turns it **Off** (it is **On** unless you turn it off); off, nothing is sent, and plays not sent yet are forgotten.

- A song you **stream** is reported when it starts. With Q2 Pod's `urls` (streaming through the player Tidal uses), a Q2 Pod that says which song plays (`now` in its capabilities) tells the app of every song in the queue, so each one is counted as it starts, the server's dashboard follows how far into it you are and whether it's paused, and it's reported stopped when the next one starts or playback stops. Jellyfin stays running while such a queue plays (with reporting on), and closes itself 30 minutes after the queue was last playing (paused or stopped) and your last press. If it was closed anyway (*Stop Streaming Services*, Rockbox), when it's opened again it picks up: the song playing then is counted, unless it was already, but songs that played from start to end while it was closed aren't, and Q2 Pod's play log doesn't have them either. With an older Q2 Pod, Jellyfin only learns which song you pressed, so the songs after it aren't counted.
- A **synced** song you play from the Q2's library, with or without Jellyfin open, is found in Q2 Pod's own play log (`.scrobbler.log` on the card, which Q2 Pod's Last.fm and ListenBrainz uploads use; Jellyfin only reads it) and reported the next time Jellyfin is open with Wi-Fi and the Q2's clock is set, with the time it was played. Plays from before reporting was on aren't sent. A song is recognised by its album and title as the server had them when it was synced; one whose tags differ from the server's is not.
- Without Wi-Fi, or when the server can't be reached, plays wait on the card (`.jellyfin/outbox.json`, up to 1,000, the oldest dropped first) and go when Jellyfin is next open with Wi-Fi, when Wi-Fi comes back, or when a sync starts. Nothing waits for them: not a song, not a page. Sign Out forgets the plays not sent. If the server took a play but its answer never arrived, Jellyfin asks the server before sending that play again, so it isn't counted twice.

**Hearts:** with a Q2 Pod that has hearts for card apps (`fav` in its capabilities), the heart on Now Playing marks the song a favourite on your Jellyfin server, and a second tap un-hearts it. It works for a song you stream from Jellyfin (the heart shows the server's favourite as the song starts) and for a synced song played from the Q2's library (there the heart is the Q2's own *My Fav*, and each change to it, from the heart, the rating picker or the hold menu, goes to the server too).

- The heart changes at once. The change waits on the card (Q2 Pod's `.jellyfin/fav.log`, then `.jellyfin/outbox.json`), through a power-off too, and goes when Jellyfin is open with Wi-Fi: favourite or not, whatever the server has. Several taps on one song send only its last state. Turning **Report Plays to Server** off doesn't affect hearts; **Sign Out** forgets the changes not sent yet.
- A heart the server refuses (it no longer has the song, or won't allow the change) isn't tried again; for a refusal, Jellyfin's main page (Settings, with swiping on) says *A favourite wasn't saved on the server* until Jellyfin closes.
- **Favourites** follows: it lists the change once the server has it. With Favourites in Sync, a song you un-heart leaves the Q2 at the next sync, as one un-hearted on the server would. A heart changed on the server shows on the Q2 the next time Jellyfin plays that song (not for a song you tapped on the Q2 since Jellyfin opened).

## Sync rules

- **The card never fills up.** Before each song, the sync checks the card: if that song would leave less than **Settings → Quality and Storage → Space to Keep Free** (256 MB, 512 MB by default, 1 GB or 2 GB), it stops there and says so: *Stopped: the card is nearly full*, how much is free, and *Free some space or sync less, then Resume*. The songs it brought are on the card, and their playlists list them. When not everything will fit, the preview says so up front, and the sync starts anyway and stops at that limit. The playback cache gives up its songs first, before the sync stops.

- An artist includes albums added to the server later. A playlist follows the server, order and duplicates included. A song reached through several items is stored once.
- Songs land at `Jellyfin/<Album Artist>/<Year> - <Album>/<Disc>-<Track> - <Title>.<ext>`, and each playlist as `_explaylist_data/<name>.m3u`, its name cut to the 63 bytes the Q2 keeps.
- Deletions happen only after the downloads have finished, and only for what the sync put there.

## What it touches

Only `Jellyfin/`, the playlist `.m3u` files it wrote and `.jellyfin/` (the playback cache is `.jellyfin/cache/`). It also reads Q2 Pod's play log (`.scrobbler.log` and `.scrobbler.log.sent`) to report plays, and never changes it; its `q2app` names `Jellyfin/` (`favdir=Jellyfin`) so Q2 Pod tells it of hearts given to synced songs. Nothing else on the card is read or changed: other apps' folders (`Subsonic/`, `Deezer/`, `Navidrome/`) and their playlists are left alone.

## Library and firmware extensions

Newer Q2 Pod firmware offers extensions to card apps. Without them everything still works, a little less smoothly:

| Extension | With it | Without it |
|---|---|---|
| `library` | After a sync the Q2 updates its library and imports the playlists itself. A playlist you made on the Q2 is never replaced: a server playlist with the same name arrives as "<name> (Jellyfin)". | Run **Library → Update Local Music**, then **Playlists → Import playlist** yourself. |
| `progress` | A real progress bar. | A text bar such as `[#####-----] 47 %`. |
| `playlist` | Streaming plays the synced songs from the card inside a playlist, and streams every format the Q2 plays as it is. | Everything in the list streams, synced or not, and only FLAC and MP3 stream as they are; anything else, and any transcode, streams as MP3 (320 kbps, or the bitrate chosen). |

## Errors and logs

A failure shows a short message on screen. Open its **Details** row for the operation, the HTTP status and the time. The full trail is in `.jellyfin/log.txt`; passwords and tokens are never logged. **Settings** shows the app's version, for a bug report.

## Update and remove

To remove it, press **Jellyfin** twice in **System settings → App Store**, or delete `.jellyfin/`. Synced music stays in `Jellyfin/`.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.jellyfin` and the release ZIP beside them. The code is Linux-only (it runs on the Q2): on Windows, build and vet with `GOOS=linux`, and run the tests under Linux or WSL. More: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). Forked from [q2-subsonic](https://github.com/andsuor/q2-subsonic), itself built from [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser); see [NOTICE](NOTICE). Jellyfin is a trademark of the Jellyfin project; this app is not affiliated with it.
