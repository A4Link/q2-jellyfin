# Changelog

All notable changes for users of q2-jellyfin. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.7.0] - 2026-11-12

### Added

- **⋯ on Now Playing shows the Play Queue** while Jellyfin's songs play, on Q2 Pod firmware that reads `more=`: the songs queued, opening at the song playing (marked *Now Playing*, or *Started here* for the song you pressed until the Q2 says what plays), the songs after it following, then *Earlier songs*; press one to play the queue from it. The Q2's own ⋯ page had Playlists and Quality Choice, which mean nothing for them.
- **Plays are reported to your Jellyfin server**, which counts them as it does for its own apps (play count, *Played*, last played). A streamed song counts when it starts; a synced song played from the Q2's library counts the next time Jellyfin is open with Wi-Fi (and the Q2's clock is set), read from Q2 Pod's play log with the time it was played. Without Wi-Fi they wait on the card. **Settings → Report Plays to Server** turns it off. With a Q2 Pod that says which streamed song plays (`now`), every streamed song is reported as it plays, with how far into it you are and whether it's paused (the server's dashboard shows it), and when it stops (Jellyfin stays running while the queue plays); with an older Q2 Pod's `urls`, only the song you pressed in a list is counted.
- **The heart on Now Playing marks the song a favourite on your Jellyfin server** (and un-hearts it), for streamed songs and for synced songs played from the Q2's library, with a Q2 Pod that has hearts for card apps (`fav`). The heart changes at once; the change waits on the card until Jellyfin is open with Wi-Fi, and only Sign Out forgets it. One the server refuses is said once on Jellyfin's main page.

### Changed

- **The Streaming icon is the Jellyfin logo in its colours** (purple to blue) in the Modern and Classic themes; Minimal shows it grey, as its other Streaming icons.
- **Sign In with Password is one page** on Q2 Pod firmware with `field`: *Username:* (the name typed beneath), *Password:* (`***` once typed) and *Sign In*. A failure says why, with *Retry* (the page again, the password cleared: it is never sent twice) and *Cancel*. *Settings → Account → Signed in as* opens the same page. What was typed stays in the app's memory only, and is forgotten once signed in or on going back. Older firmware keeps the pages it had.
- **The server's address is a *Server Address:* row** on the first page and the Server page on Q2 Pod firmware with `field`: an address that doesn't work says why, with *Retry* and *Cancel*, instead of a page to press Return past.
- **No *Stop Jellyfin* row** on Jellyfin's Loading… and error pages on Q2 Pod firmware that reads `stop=0`: an error page has *Retry* and *Cancel*.
- **A sync takes a song already in the playback cache** at Sync Quality by moving it, instead of downloading it again, and Start Sync no longer clears from the cache the songs it is about to take.

### Fixed

- **Albums sorted Random opens on a large library:** it says *Loading albums…* while every album comes from the server, then shows them shuffled, instead of timing out each time Albums was opened.
- **A list that can't load still shows its Search and Sort** (Albums, Artists and Playlists, in any sort), so a sort that keeps failing can be changed there.

## [0.6.2] - 2026-11-12

### Changed

- **Playing a song, album or playlist opens Now Playing at once**: the black "Playing …" message shown first is gone. A song that has to download first still says "Buffering …".
- **Default Quality is now Sync Quality**, and it is every synced song's: the *Quality* rows on an album, artist, playlist, Favourites, a song and Entire Library are gone, and a quality saved for one item is no longer used. After a change of Sync Quality, the next sync brings the songs already on the Q2 again at the new quality.
- **Sync Quality and Streaming Quality list Original first, then Opus from 320 kbps down.** Streaming Quality no longer has *Same as Default Quality*: one left at it keeps the quality it had.
- **Settings → Account** shows the server, *Signed in as* and *Sign Out* (and, on a Q2 signed in with an API key, that an admin-level key is stored); what is stored on the card, and the tip to use a server user just for the Q2, are in the README.
- **Add to Sync answers at once** on an album's, artist's, playlist's or Favourites' page, and for a song on such a page: it uses the list the page was drawn from instead of asking the server for it again.
- **API key sign-in is turned off for now:** *Sign In with an API Key* is no longer offered, and a `Jellyfin sign-in.txt` with `apikey=` is deleted and refused with "API key sign-in is turned off for now". A Q2 already signed in with a key stays signed in.

### Fixed

- **Changing Sync Quality during a sync** starts that sync again at the new quality, so the songs it hadn't fetched yet no longer come at the old one. A paused sync stops instead and waits for *Start Sync* (holding an item in Sync Jobs while paused does the same), and choosing the quality already set changes nothing.
- **Changing Sync Quality after a finished sync** opens a new preview on the Sync page, with *Start Sync*, instead of the old *Sync complete* page.
- **A setting that can't be saved** (Sync Quality, Streaming Quality, Space to Keep Free, Playback Cache) says *Couldn't save the setting* instead of showing the new value.

## [0.6.1] - 2026-11-11

### Fixed

- **A typed password or API key is never a page's title, with or without Wi-Fi:** after signing in the page says *Login Success*; a wrong one says why. Q2 Pod titles a typed text's page with the text.
- **Retry on a wrong password or API key** no longer sends it to the server again (Jellyfin can lock out a user after failed sign-ins): it says why again; Return asks for another. A server that couldn't be reached is still tried again.
- **Retry on a page kept from before the app closed** sends nothing to the server: a password or key typed there is never searched for or shown; the page says *Press Return and search again*.
- **Return after signing in no longer says "No playlists called that"** or opens Sign In with Password again: the sign-in pages Return goes back through show Newly Added, Albums, Artists, Playlists and Settings, and the typed user name or password is never searched for.
- **Return from the user name's page to the server address's** shows the ways to sign in, not *User: https://…* (a password typed next was sent with the address as the user name).
- **Return after a server address that didn't work** goes back to the page it was typed on, however many were tried; the message says to press Return to try another.
- **Quick Connect says where to approve the code:** *User Menu → Quick Connect* (it said Settings).
- **Swiping back to Albums, Artists or Playlists** returns to the page and row you left, instead of the first page with a different row selected. Back to the main list (Swipe Between Sections off) still opens each list at its start.
- **A library update the Q2 never answers** no longer keeps the app running indefinitely: it may close two hours after asking.
- **Lyrics while streaming stop asking an unreachable server:** after two tries that can't reach it, the rest of the queue's lyrics wait for the next Play, instead of each song waiting up to 3 s in turn.
- **The clock from the server is checked against its certificate:** on a Q2 whose clock is unset, the time is now taken only when it falls within the server certificate's validity, and never before 2025, so an expired certificate can't set the clock back.
- **A search typed on Albums searches Albums** even after leaving an Artists or Playlists list that was still loading; before, the slow list could finish afterwards and make the search run there.
- **Sign Out wipes the stored sign-in** before deleting it, so it can't be recovered from the card.
- **Entire Library no longer removes songs** of an album the server's album list skipped while it changed during the preview.
- **A music library with more than one folder** keeps every folder's songs in playlists and syncs (only the first 200 top-level items were read).
- **The same song in two formats** gets its own lyrics file instead of sharing one.

### Changed

- **Sign Out** shows the sign-in once it has signed out.
- **Signed out, Change Server is first**, then Quick Connect, password and API key; it opens the Server page.
- **Settings → Account → Server:** the address is the row to press to type a new one, what that does beneath it.
- **Settings → Account → Signed in as** signs in as another user with a password; the sign-in now stays until the new one works.
- **Waiting for Quick Connect** shows only the *Code*, where to approve it and Cancel, which brings back the other ways to sign in.
- **The server's address** shows with its `https://`, and the box to type one says `https://` is added.
- **Settings is shorter:** On This Q2 and Sync Entire Library are now on the Sync page, whose options read Sync Jobs, On This Q2, Sync Favourites, Choose Playlists, Sync Entire Library, and stay at the top of the page whatever the sync is doing (running, paused, stopped, finished, or the server out of reach), with the sync's progress or result below them. Default Quality, Streaming Quality, Space to Keep Free, Playback Cache and Clear Playback Cache moved to a new **Quality and Storage** page, just below Sync in Settings.
- **Less battery and Wi-Fi use:** one cover download per album instead of per song row; lyrics kept between Plays and read from the card for synced songs, and no more tries once the server is out of reach; Play reuses the list just shown; Artists, Playlists and Newly Added reuse their list for a minute; fewer log lines and manifest writes; connections reused, then closed when idle; a failed cover waits a minute before another try; the app wakes every 5 s instead of every second when nothing is under way.
- **Settings' Connected line** now follows the app's last real request to the server (Not connected after one that failed or was refused) instead of pinging the server every 30 s.

### Added

- **Sorting Albums, Artists and Playlists**: a Sort page with *Sort By* and *Order*. Albums by Name, Release Date, Date Added, Album Artist or Random; Artists by Name, Date Added or Random; Playlists by Name, Date Added, Duration or Random (Favourites stays first). Each list keeps its choice. Random keeps its order while you scroll and come back; Order shuffles again. With Q2 Pod's `sort` cap the Sort page opens from an icon in the title bar, beside the search icon; otherwise from a *Sort* row at the top of the list.
- **Streaming through the player Tidal uses** (v0.6, with Q2 Pod 1.7.3's `urls` cap): a streamed song plays from the server as it arrives, with its cover in Now Playing, instead of being saved to the card first. On older firmware streaming works as before.
- **Covers in lists** (v0.6, with Q2 Pod 1.7.3's `art` cap): albums, artists and playlists show their cover beside each row.
- **Lyrics while streaming** (v0.6, with Q2 Pod's streamed-lyrics change, not yet in a release): Now Playing shows the server's lyrics for a streamed song, as for a synced one.
- **Search on Newly Added** (v0.6): a search of artists, albums and songs. With Q2 Pod 1.7.3, every section's search is an icon in the title bar.
- **Sync Jobs** (v0.6), on the Sync page and under a running sync: what is in Sync and not yet on the Q2. A press holds an item: it keeps its songs on the Q2 but adds no more until resumed, and a sync under way starts again without it, so the rest goes first.
- **The songs a sync adds** (v0.6): Sync's "+N songs" row opens their list.
- **Long lists a page at a time** (v0.6, with Q2 Pod 1.7.3's `window` cap): Artists, Albums, Playlists, search and an item's songs show 200 rows, then a *Next page* row; a later page starts with *Previous page*. Each loads in place, so Back leaves the list rather than paging back, and coming back from Now Playing reloads only that page. Later pages reuse the artist and playlist list for a minute instead of fetching it again.
- **Artists show their number of albums** (v0.6): it was always 0.
- First scaffold: a Jellyfin client (`internal/jellyfin`) for browsing, search, sync and streaming, with sign-in that keeps only an access token on the card, and Quick Connect support in the client.
- Sign-in on the Q2 itself, in order of preference: Quick Connect (no password reaches the Q2), user name and password typed on the Q2 (sent only to the server over HTTPS, never stored), or an admin-level API key with a warning. The server's address is typed on the Q2 too.
- **Quality**: Original (the server's file, no transcoding) by default, or an Opus transcode made by the server at 64, 96, 128, 160, 192, 224, 256, 288 or 320 kbps, chosen from a list for the default, for streaming and per item. Originals the Q2 can't play arrive as Opus 320. (Replaces q2-subsonic's MP3 320/128.)
- **Streaming starts before a song has all arrived** (v0.2; v0.4 below makes it more careful).
- **Lyrics from the Jellyfin server**, synced or plain, beside each synced or streamed song for Q2 Pod's Now Playing.
- **New layout**: Newly Added, Albums, Artists, Playlists and Settings; a search box at the top of Albums, Artists and Playlists for that section; Favourites at the top of Playlists; Sync, On This Q2 and Sync Entire Library under Settings.
- **Buffering feedback** (v0.5, replacing v0.4's buffering page): a *Buffering…* message, and the pressed song's row shows the progress until it plays; no extra page, so Return from Now Playing goes straight back to the list.
- **Next works on streamed songs again** (v0.5): the songs after the one playing start as early as their link allows (as the first does), so Q2 Pod queues them and Next can go to them; v0.4 waited for them to be whole, so Next did nothing until then.
- **Steadier streaming** (v0.4): only a FLAC or WAV original starts before it is whole, after 3 s on a connection at least twice the song's rate, otherwise after at least 15 s and enough to play through at 60 % of the measured speed (the slower of the whole download's and the last 3 seconds'). Transcodes and other formats play once whole, so their length and seeking are right.
- **Settings → Swipe Between Sections** (v0.4): on by default where the firmware offers sections; off on request; turned off when the card moves to firmware without them, and greyed out (unavailable) there.
- **Swipe between sections** (v0.3) on Q2 Pod firmware with the `sections` cap: each of the five sections is a page, and a sideways swipe changes page. Stream errors show at the top of Settings.
- **Sign-in**: after typing the server address, the sign-in choices and Change Server show at once.
- **Music only**: lists, search, favourites, playlists and sync are held to the server's Music libraries; films, audiobooks and mixed-content files never appear.
- **Sync Favourites** (one press adds your favourites and starts the sync) and **Choose Playlists** on the Sync page.
- **Space to Keep Free** (Settings, 512 MB by default): a sync stops before the card falls under it, says why, and keeps what it brought; Resume continues. A sync that doesn't all fit no longer refuses to start.
- Tested against a real Jellyfin 12.4.0 server: `contrib/testserver/` makes one in Docker with synthetic media.
- **Favourites**: your heart-marked Jellyfin songs, to play or to sync as a **Favourites** playlist.
- **Playback cache**, as Q2 Pod's Tidal cache: streamed songs stay on the card once played and replay without Wi-Fi. Size in Settings (Off to 8 GB, 2 GB by default), oldest played first out, **Clear Playback Cache**; it gives way to a sync that needs the space.
- Everything else from q2-subsonic: the Q2 Pod card-app integration, sync preview and engine, playlists, streaming, logs.

### Removed

- q2-subsonic's phone sign-in page (a port on the Wi-Fi with a QR code and PIN): the app opens no port; everything is typed on the Q2.
- q2-subsonic's one-time `Navidrome/` folder migration: this app never touches another app's files.

[Unreleased]: https://github.com/A4Link/q2-jellyfin/commits/master
[0.7.0]: https://github.com/A4Link/q2-jellyfin/commits/q2-v0.7.0
[0.6.2]: https://github.com/A4Link/q2-jellyfin/commits/q2-v0.6.2
[0.6.1]: https://github.com/A4Link/q2-jellyfin/commits/q2-v0.6.1
