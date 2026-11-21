> **This app is 100% vibecoded.** It works, and every change was reviewed and tested, but the quality of its code cannot be assured. Use it at your own risk.

# DOCUMENTATION TBC


# q2-jellyfin

A [Jellyfin](https://jellyfin.org) client for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Pick playlists, albums, artists and songs on the Q2, and it syncs them to the card over Wi-Fi, or streams a song straight from your server. It needs **Q2 Pod 1.7.3 or later** and a Jellyfin server reachable over HTTPS (or, on your home network, plain HTTP at its IP address). Its requests are checked against the Jellyfin 12.4.0 API document.

> **Status: 0.8.** Its tests run against a real Jellyfin 12.4.0 server, and test builds have run on a Q2 with Q2 Pod 1.8.0. It needs Q2 Pod 1.7.3 or later: on an older Q2 Pod it shows only *Needs Q2 Pod 1.7.3 or later. Update Q2 Pod to use Jellyfin.* A few features (the one-page sign-in, ⋯ showing the play queue, the heart, and reporting every streamed song) need Q2 Pod 1.8.1 or later; on 1.7.3 to 1.8.0 the app keeps its older pages and behaviour. Forked from [q2-subsonic](https://github.com/andsuor/q2-subsonic).

## Features

- **Browse:** Newly Added, Albums, Artists, Playlists and Favourites from your Jellyfin server, with covers, search and sorting by the server (Random shuffles the server's list on the Q2).
- **Stream:** play songs straight from the server. Now Playing's ⋯ shows the play queue.
- **Sync:** open a playlist, album, artist or Favourites and press **Add to Sync**, or sync the whole library. Synced songs play offline. Before a sync starts, a preview lists what will be added and removed. A sync pauses before the card's free space drops below the amount you set.
- **Quality:** the server's original files, or Opus at a bitrate you choose. Songs that are already lossy (MP3, AAC, Opus) are kept as they are unless Always Re-encode is on.
- **Favourites:** the heart on Now Playing sets or clears a Jellyfin favourite. Favourites can be synced as a playlist that follows your hearts.
- **Play reporting:** plays on the Q2, streamed or synced, are reported to your server; plays made offline go once Jellyfin is open with Wi-Fi. Some plays can't be counted, such as songs that play through while Jellyfin is closed: see *Reporting plays*.
- **Lyrics:** shown with the song when the server has them. Played from Jellyfin on Q2 Pod 1.7.4 and later, they come for the song you press and the 19 after it.
- **Secure and frictionless:** sign in with Quick Connect, by approving the Q2 from another Jellyfin app, or with a username and password. Only an access token is stored, encrypted. All connections to the server use HTTPS, unless you choose a home network server's plain HTTP address, and the Q2's player never receives the token.
- **Music only:** films, audiobooks and photos on the same server are left out.

## Install

**Over Wi-Fi:** once listed in Q2 Pod's App Store (`sources.list`), open **System settings → App Store** and press **Jellyfin**.

**By hand:**

1. Download `q2-jellyfin-*.zip` from the latest release.
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.jellyfin/q2-jellyfin`, `.jellyfin/run`, `.jellyfin/q2app` and its icons `.jellyfin/icon.png` and `.jellyfin/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

**Jellyfin** then has a row in **Streaming**.

## Sign in

Everything can be done on the Q2 itself. The app serves no web page and opens one port, on the Q2's own loopback (127.0.0.1) only, which nothing on your network can reach: Q2 Pod's player streams through it, over the app's own verified HTTPS connection. Nothing the app does crosses your Wi-Fi except HTTPS to your server (or HTTP to a home network server, below).

**First, the server's address.** Open **Streaming → Jellyfin** and press **Type Your Server Address**. Type it on the Q2's keyboard, for example `music.example.com` or `music.example.com:8920` (`https://` is added for you). A server on your home network without HTTPS can be typed as its IP address and port, for example `192.168.1.10:8096` or `192.168.1.10:8096/jellyfin`: see **A server at home without HTTPS** below. The Q2 checks it is a Jellyfin server before keeping it. **Change Server** at the top of the sign-in list opens the address: press it to type a new one. On Q2 Pod firmware with `field` (in `/tmp/q2-addon.caps`) the address is a **Server Address:** row instead, at the top of the sign-in list too: press it and type; an address that works brings you back to the sign-in list (signed in, a new address on **Settings → Account → Server** signs you out and does the same), with your last user name already under **Username:** on the password page. A server that can't be reached, or doesn't answer as Jellyfin, says why, with **Try Again** to check the same address again and **Server Address:** to type another; something that isn't an address says why, with **Server Address:** to type it again (on Q2 Pod with `fieldtext` the box holds what you typed).

**Then sign in on the Q2, in this order of preference:**

1. **Sign In with Quick Connect** (recommended). The Q2 shows a six-digit code. In any Jellyfin app where you're already signed in (the web page, a phone app), open **User Menu → Quick Connect** and enter it. The Q2 signs in on its own. Your password never reaches the Q2. Your server's admin must have Quick Connect turned on (**Dashboard → General**).
2. **Sign In with Password.** Type your user name, then your password, on the Q2's own keyboard. The password goes only from the Q2 to your server over HTTPS, is swapped there for an access token, and is never stored. Q2 Pod's keyboard has no hidden-text mode, so the password shows on screen while you type it; once typed it is never shown (the page says *Login Success*). A wrong password is sent once: Retry says why again, and Return asks for another. A password the same as the user name can't be typed this way (the Q2 takes it for the user name): use Quick Connect.

   On Q2 Pod firmware with `field` (in `/tmp/q2-addon.caps`), it is one page instead: press **Username:** and type your user name, which then shows beneath it; press **Password:** and type your password, which then shows as `***`; then press **Sign In**. A sign-in that fails (a wrong password, the server out of reach) says why, with **Retry**, which brings the page back with the password cleared, and **Cancel**. The password is tried once and then forgotten, so it is never sent twice: type it again to try again. What you typed is kept only in the app's memory, never on the card, and is forgotten once you sign in or go back (to the sign-in list, or to Account). With no Wi-Fi, Sign In sends nothing and the password stays for when Wi-Fi is back. On this page a password may be the same as the user name. **Settings → Account → Signed in as** opens the same page, to sign in as another user; the sign-in you have stays until the new one works.

Sign-in with a Jellyfin API key is turned off for now: a Jellyfin API key is **admin-level** (it belongs to no user and opens every library on the server), so the Q2 doesn't offer it. A Q2 already signed in with one stays signed in.

**Or from a computer:** put a file named `Jellyfin sign-in.txt` in the card's top folder, with the lines `server=music.example.com`, `user=alice` and `password=…`. The next time Jellyfin is open with Wi-Fi, it deletes the file, whether the sign-in works or not, then signs in with it (in place of the sign-in you have, once the new one works); the main page says how it went. Until then the password is on the card as plain text. On Q2 Pod before 1.7.3 the file is read and deleted only once Q2 Pod is updated, so update Q2 Pod first. Copy the file with the card in a computer, not with Wi-Fi Transfer: it sends the file unencrypted, and while Wi-Fi Transfer is on anyone on the Wi-Fi can read the card. Then open Jellyfin with Wi-Fi straight away, so the file is used and deleted.

Things to know:

- The server must be `https://`, unless it is on your home network: see **A server at home without HTTPS** below. Certificates are verified, and the token is never sent to another host on a redirect. Songs that Q2 Pod's own player streams are fetched through the app too, so they get the same checks, and no token is written to the queue Q2 Pod plays.
- **Only an access token is stored on the card**, in `.jellyfin/credentials`: the token from Quick Connect or a password sign-in, never a password. It is encrypted with a key kept on the Q2 itself, so the card on its own doesn't give it away, but whoever has both can use it. If the Q2's own storage can't take the key (it is full or read-only), the sign-in stays unencrypted until it can; `.jellyfin/log.txt` says so. In another Q2 the card's sign-in can't be read, so Jellyfin asks you to sign in again; revoke the old one under **Dashboard → Devices**. **Tip: use a Jellyfin user just for the Q2, not an admin**, so a lost Q2 opens only that user's music. If it is lost, revoke the Q2 under **Dashboard → Devices**. A Q2 whose sign-in the server no longer takes (revoked, or the server rebuilt) says *Server refused this Q2*: sign in again after **Settings → Account → Sign Out**. A Q2 signed in with an API key before this version keeps that key on the card instead; if that card or Q2 is lost, delete the key under **Dashboard → API Keys** (revoking the device doesn't cancel an API key).
- **Settings → Account** shows the server and who is signed in; press **Signed in as** to sign in as another user with a password; **Sign Out**, pressed twice (the first press says *Press again to sign out*, as it stops a sync and the music), removes the token and shows the sign-in. Your music stays on the card.
- Syncing in **Original** quality needs the user's **Allow media downloading** permission on the server; Opus transcodes don't. Under an Opus quality, songs that are already lossy (MP3, AAC, Opus…) still sync as the server's file, which needs the permission, unless **Always Re-encode** is on.

On Q2 Pod firmware that has them, Jellyfin's Loading… and error pages have no **Stop Jellyfin** row (an error page has **Retry** and **Cancel**, Cancel going back as Return does), and **⋯** on Now Playing, while Jellyfin's songs play, opens the **Play Queue** instead of the Q2's own ⋯ page: the songs queued to play (up to 100 before the one you pressed and 400 from it on). It opens at the song playing, with the songs after it following, then *Earlier songs*. The song playing is marked *Now Playing* once the Q2 says it plays (a Q2 Pod with `now`); until then, and on an older Q2 Pod, the song you pressed is marked *Started here*. Press a song to play the queue from it. It needs no Wi-Fi to show; after the Q2 is switched off, or after Sign Out, it says *Nothing from Jellyfin is playing* until you play something again. Older firmware ignores both.

## A server at home without HTTPS

Jellyfin serves plain HTTP on its own port (8096) and HTTPS only if you set it up. On your home network the Q2 can use the HTTP port. **Plain HTTP is not encrypted:** anyone on the same network can read your password while you sign in and the access token on every request, and the token is your Jellyfin account. The Q2 asks you before using a server over HTTP, and sends your sign-in to it only on your own Wi-Fi.

**1. Find the server's address.** In Jellyfin's web page open **Dashboard → Networking**. You need three things:
- the server's **IP address** on your network, for example `192.168.1.10` (also shown by your router; give the server a fixed address there, so it doesn't change);
- its **HTTP port**, `8096` unless someone changed it (`8920` is the HTTPS port, which works only once HTTPS is set up);
- its **Base URL**, if one is set (for example `/jellyfin`). Many setups have none.

You can check them from a computer on the same network: `http://192.168.1.10:8096/System/Info/Public` in a browser (with the Base URL after the port: `http://192.168.1.10:8096/jellyfin/System/Info/Public`) shows a short text with `"ProductName":"Jellyfin Server"`. If it shows Jellyfin's web page or nothing, the port or the Base URL isn't right.

**2. Type it on the Q2**, as the IP address, the port and the Base URL if there is one, with no `http://` (it is added for you):
- `192.168.1.10:8096`
- `192.168.1.10:8096/jellyfin` (with the Base URL `/jellyfin`)
- `192.168.1.10:30013` (any other port your server uses)

Always type the port. A name such as `nas.local:8096` means HTTPS, which Jellyfin's HTTP port doesn't speak: use the IP address.

**3. Answer the question.** The Q2 checks the address is a Jellyfin server (sending nothing secret), then says *192.168.1.10:8096 is served over HTTP*, *Not encrypted* and *Say Yes only at home*, with **Yes, allow HTTP** and **No**. Answer it at home. Yes adds the server to the allow list (below), so it isn't asked again, and keeps your Wi-Fi as the server's home network. Then sign in. **Quick Connect is the best choice here:** no password is sent. Use a Jellyfin user just for the Q2, not an administrator.

**Your home Wi-Fi only.** The Q2 keeps the Wi-Fi's name and the router's MAC address with the server, for up to 4 Wi-Fis (a 5 GHz name, an extender). On any other Wi-Fi (a hotel's, a café's, where 192.168.1.10 can be some other device) it sends your sign-in nowhere and says *Not this server's home Wi-Fi*. At home on another of your Wi-Fis, or after a new router, type the address again: the Q2 asks *This Wi-Fi isn't saved for 192.168.1.10:8096* / *Say Yes only at home*. Say Yes only at home: elsewhere a device could pose as your server. Just after Wi-Fi wakes, the Q2 may not read the network yet: it sends nothing and says it can't reach the server; try again a moment later.

**The allow list** is `.jellyfin/http-allow.txt` on the card, one address per line:
- a range, such as `192.168.0.0/16`: an IP address and port in it, typed on the Q2, means HTTP. The file starts with the home ranges `10.0.0.0/8`, `172.16.0.0/12` and `192.168.0.0/16`;
- an IP address and port, such as `192.168.1.10:8096`: a server you said Yes to (the Q2 adds these).

Edit it on a computer: add a range (Tailscale's `100.64.0.0/10`, say), or delete a server's line to stop the Q2 sending it anything (it then says *Not on http-allow.txt: nothing sent*). Leave no address in it to turn plain HTTP off; delete the file to get the defaults back (that also forgets the servers you said Yes to: type a server's address again on the Q2 and say Yes, or add its line back, e.g. `192.168.1.10:8096`). `http://` typed in full on the Q2 works for any IPv4 address and port, even one outside the list; outside the home ranges and Tailscale's `100.64.0.0/10` (which the Q2 reaches through your own router) the question says *On the internet, NOT encrypted*: your sign-in would cross the internet readable, so use HTTPS for that.

**Signing in from a computer** (`Jellyfin sign-in.txt`) works for an HTTP server only once its address has been typed on the Q2 and said Yes to, on the Wi-Fi the Q2 is on.

**What the messages mean:**

| The Q2 says | What to do |
|---|---|
| *Add the port, e.g. 192.168.1.10:8096* | Type the port after the IP address. |
| *Not a Jellyfin server* | Something else answers there, or Jellyfin has a Base URL: add it after the port (`192.168.1.10:8096/jellyfin`). |
| *The server redirected to another address. Check the address and port* | Usually the wrong port: another service answers there. |
| *This port has no HTTPS. At home, type the server's IP and port* | A name was typed with Jellyfin's HTTP port: type the server's IP address and port instead. |
| *No HTTPS on this port. Type http:// first* | An IP address outside the allow list's ranges (Tailscale's, say) was typed with its HTTP port: type `http://` in front, or add its range to `http-allow.txt`. |
| *Can't reach 192.168.1.10:8096* | The server is off, the IP or port is wrong, or the Q2 is on another network. |
| *Can't check this Wi-Fi yet. Try again in a moment* | The Q2 couldn't read the router's address yet: try again. |
| *Not this server's home Wi-Fi…* | The Q2 is on a Wi-Fi not saved for the server: nothing was sent. |
| *Plain HTTP is turned off in http-allow.txt* | The allow list has no address: add one, or delete the file. |
| *http-allow.txt has no address the app can read* | The allow list has lines, but none is an address the app can read (a typo, or the file saved as UTF-16 "Unicode"): fix the line, save the file as UTF-8, or delete it. Plain HTTP is off until then. |
| *No HTTPS on this port. Plain HTTP is turned off in http-allow.txt* | An IP address was typed on a port with no HTTPS, and the allow list has no address: use the server's HTTPS port, or add an address to `http-allow.txt` (or delete the file). |

**Or use HTTPS instead:** the safe way, at home and away. Put a reverse proxy (Caddy, Nginx Proxy Manager, Traefik) in front of Jellyfin with a certificate from a public authority (Let's Encrypt) for a name you own, and type that name on the Q2. A self-signed certificate isn't accepted.

## Use

**Music only.** A Jellyfin server can also hold films, shows, audiobooks, photos and mixed-content folders. q2-jellyfin only ever lists, searches, syncs and streams songs from the server's **music libraries** (libraries of the Music type): no film, audiobook or file from another kind of library appears, even when it is a favourite or sits in a playlist. To make something available on the Q2, put it in a Music library on the server.

The app opens on its sections: **Newly Added** (the latest albums), **Albums**, **Artists**, **Playlists** and **Settings**. Albums, Artists and Playlists each have a search box at the top that searches that section (Albums finds albums and songs), and **Sort** below it (an icon in the title bar on Q2 Pod firmware with `sort`): **Sort By** Name, Date Added or Random (Albums also Release Date and Album Artist, Playlists also Duration) and **Order** (A to Z, Newest first…), kept for each list. **Settings** has **Sync** at the top (its subtitle on the main page shows how the sync stands), then **Quality and Storage**, **Swipe Between Sections**, **Report Plays to Server** (see *Reporting plays* below), **Account** and the version. Each section is a page of its own, and a sideways swipe moves to the next or previous one. **Settings → Swipe Between Sections** turns this off and on (on by default; it applies when Jellyfin is next opened): off, the sections are rows of the main page.

**Favourites**, at the top of **Playlists**, lists the songs you've marked with the heart in Jellyfin, like Liked Songs or Tidal's My Collection. Play it, or add it to Sync like a playlist: its songs are synced and a **Favourites** playlist appears on the Q2, following your hearts at each sync.

**Choosing what to sync:** open a playlist, album or artist and press **Add to Sync** / **Remove from Sync**. While something in Sync (and not held) isn't on the Q2 as chosen, or something taken out of Sync is still there, lists start with **Sync Now · N waiting**, which opens Sync.

**Quality.** By default every song is the server's own file, untouched: **Original (no transcoding)**. To save space or data you can choose an **Opus** transcode instead, at 320, 288, 256, 224, 192, 160, 128, 96 or 64 kbps; the Jellyfin server does the converting, and the Q2 receives Opus files. Both qualities are set in **Settings → Quality and Storage**: **Sync Quality** is every synced song's, and **Streaming Quality** is what you stream. Each opens a list of the choices, Original first, then Opus from 320 kbps down, with the current one marked **Selected**. When you change Sync Quality, the next sync brings the songs already on the Q2 again at the new quality (the preview shows how much).

**Always Re-encode** (Settings → Quality and Storage, off by default) decides what an Opus quality does to songs that are already lossy: MP3, AAC (including AAC in .m4a), Ogg Vorbis, Opus, WMA and AC-3. Off, they come as the server's file, untouched: encoding a lossy file again only loses more, and saves less than its bitrate suggests. On, they are re-encoded to the chosen Opus rate too, for smaller files at a lower quality. Lossless songs (FLAC, ALAC, WAV, APE…) are transcoded either way. The row says underneath what its setting means. Under an Opus Sync Quality, turning it on or off changes what the next sync brings, as a Sync Quality change does; under Original it changes only what an Opus Streaming Quality streams.

One exception is automatic: an original the Q2 can't play (WavPack, Musepack, …) arrives as Opus 320.

**Syncing:** open **Settings → Sync**. Its options are always at the top of the page, whatever the sync is doing:

- **Sync Jobs**: what is in Sync and not yet on the Q2; press one to hold it or resume it.
- **On This Q2**: everything chosen, with its status and its size on the card.
- **Sync Favourites**: one press adds your favourites (the songs you've hearted in Jellyfin) to Sync and starts the sync. Once they're in Sync, the same press syncs again, picking up new hearts, and resumes Favourites if they are held in **Sync Jobs**. A sync that would take songs off the Q2 doesn't start by itself: the preview shows what goes, and waits for **Start Sync**.
- **Choose Playlists**: your playlists, each marked **In Sync** or not; press one to add it or take it out. Each comes to the Q2 as a playlist, in the server's order, duplicates included.
- **Sync Entire Library**: press to sync every album and playlist (On, with the counts once checked), or press again to take it out of Sync (Cancel stops a sync under way).

Everything synced is at **Sync Quality** (Original, or the Opus rate you chose).

Below them, Sync shows a preview, and nothing happens until you press **Start Sync**. An item the server no longer has is shown as *<name> · no longer on the server* (the first five by name, then *and N more no longer on the server*); its songs are listed under Removed (**Sync Jobs** and **On This Q2** show it as *No longer on the server*), and **Start Sync** takes it out of Sync. The server answers the same for an item of a music library your account can no longer see, and refuses a list of one (Entire Library, Favourites, an artist), so then the preview checks the libraries again: with none left, it says *No music library on the server for this account* and removes nothing. Once started, it shows progress with **Pause**, **Resume** and **Cancel**. If Wi-Fi drops, the sync pauses until you press **Resume**; if the Q2 restarts or Jellyfin closes, press **Start Sync** again. Either way it picks up where it was: songs already on the Q2 aren't downloaded again. Songs that fail are counted while the sync runs, and listed under **Failed Songs** (the first 500), with **Retry Failed**, also when a sync stops or pauses. After a song fails, the next one is tried once, without waiting, so a server that fails every song doesn't keep a sync going for hours. When the sync would remove more than half the synced songs, the preview says *Removes most of the synced songs*.

**Playing:** press a song to play it and the rest of the list after it. Q2 Pod's own player streams the songs: Now Playing opens at once, with the player's spinner while a song arrives. A song the server can't send stops playback there, with no message on screen; `.jellyfin/log.txt` says why.

**Lyrics:** when your Jellyfin server has a song's lyrics (a .lrc file beside it on the server, or lyrics in its tags), they show on Now Playing, lit line by line when they're timed. A sync puts them on the card beside the song. When you press a song in Jellyfin on Q2 Pod 1.7.4 or later, that song and the 19 after it get their lyrics (a synced song's are read from the card); a song further on gets them once a later press reaches it. On Q2 Pod 1.7.3, songs streamed from the server show none. They only ever come from your server.

**Playback cache:** earlier versions kept songs played on Q2 Pod before 1.7.3 in `.jellyfin/cache/`. Nothing is added to it now, and a sync doesn't use it. While it holds anything, **Settings → Quality and Storage → Clear Playback Cache** (press twice) removes all of it, leftover part files too.

**Reporting plays:** your Jellyfin server counts what you play on the Q2, as it does for its own apps: the song's play count, *Played* mark and last-played date, which the server's lists sort and filter by. **Settings → Report Plays to Server** turns it **Off** (it is **On** unless you turn it off); off, nothing is sent, and plays not sent yet are forgotten.

- A song you **stream** is reported when it starts. A Q2 Pod that says which song plays (`now` in its capabilities) tells the app of every song in the queue, so each one is counted as it starts, the server's dashboard follows how far into it you are and whether it's paused, and it's reported stopped when the next one starts or playback stops. Jellyfin stays running while such a queue plays, if it streams any song (a queue of only synced songs doesn't keep it running), and closes itself 30 minutes after the queue was last playing (paused or stopped) and your last press. If it was closed anyway (*Stop Streaming Services*, Rockbox), when it's opened again it picks up: the song playing then is counted, unless it was already, but songs that played from start to end while it was closed aren't, and Q2 Pod's play log doesn't have them either. With an older Q2 Pod, Jellyfin only learns which song you pressed, so the songs after it aren't counted.
- A **synced** song you play from the Q2's library, with or without Jellyfin open, is found in Q2 Pod's own play log (`.scrobbler.log` on the card, which Q2 Pod's Last.fm and ListenBrainz uploads use; Jellyfin only reads it) and reported the next time Jellyfin is open with Wi-Fi and the Q2's clock is set, with the time it was played. Plays from before reporting was on aren't sent. A song is recognised by its album and title as the server had them when it was synced; one whose tags differ from the server's is not.
- Without Wi-Fi, or when the server can't be reached, plays wait on the card (`.jellyfin/outbox.json`, up to 1,000, the oldest dropped first) and go when Jellyfin is next open with Wi-Fi, when Wi-Fi comes back, or when a sync starts. Nothing waits for them: not a song, not a page. Sign Out forgets the plays not sent. If the server took a play but its answer never arrived, Jellyfin asks the server before sending that play again, so it isn't counted twice.

**Hearts:** with a Q2 Pod that has hearts for card apps (`fav` in its capabilities), the heart on Now Playing marks the song a favourite on your Jellyfin server, and a second tap un-hearts it. It works for a song you stream from Jellyfin (the heart shows the server's favourite as the song starts) and for a synced song played from the Q2's library (there the heart is the Q2's own *My Fav*, and each change to it, from the heart, the rating picker or the hold menu, goes to the server too).

- The heart changes at once. The change waits on the card (Q2 Pod's `.jellyfin/fav.log`, then `.jellyfin/outbox.json`), through a power-off too, and goes when Jellyfin is open with Wi-Fi: favourite or not, whatever the server has. Several taps on one song send only its last state. Turning **Report Plays to Server** off doesn't affect hearts; **Sign Out** forgets the changes not sent yet.
- A heart the server refuses (it no longer has the song, or won't allow the change) isn't tried again; for a refusal, Jellyfin's main page (Settings, with swiping on) says *A favourite wasn't saved on the server* until Jellyfin closes.
- **Favourites** follows: it lists the change once the server has it. With Favourites in Sync, a song you un-heart leaves the Q2 at the next sync, as one un-hearted on the server would. A heart changed on the server shows on the Q2 the next time Jellyfin plays that song (not for a song you tapped on the Q2 since Jellyfin opened).

## Sync rules

- **The card never fills up.** Before each song, the sync checks the card: if that song would leave less than **Settings → Quality and Storage → Space to Keep Free** (256 MB, 512 MB by default, 1 GB or 2 GB), it pauses there and says so: *Paused: the card is nearly full*, how much is free and what the next song needs, and *Free some space or sync less, then Resume*; with songs in the playback cache, also *Clear Playback Cache frees* and how much. The songs it brought are on the card, and their playlists list them. When not everything will fit, the preview says so up front, and the sync starts anyway and pauses at that limit. A Sync Quality change replaces songs before it adds new ones, so a smaller format frees room first; a replace that makes its song smaller still goes ahead when it fits, and can take the card under Space to Keep Free by that one song until its old file is deleted.

- An artist includes albums added to the server later. A playlist follows the server, order and duplicates included. A song reached through several items is stored once.
- Songs land at `Jellyfin/<Album Artist>/<Year> - <Album>/<Disc>-<Track> - <Title>.<ext>`, and each playlist as `_explaylist_data/<name>.m3u`, its name cut to the 63 bytes the Q2 keeps. A playlist taken out of Sync has its `.m3u` deleted, but the Q2's own copy, imported from it, stays (Q2 Pod can't delete one): delete it in **Library → Playlists**. Added back to Sync while that copy is still there (Entire Library turned on again too), the playlist arrives as "<name> (Jellyfin)".
- Songs no longer in Sync are deleted after the downloads, or as soon as the card runs short of room, and only what the sync put there. The deletions are held back only when songs failed in a way that looks server-wide (under Entire Library, only when no song arrived after it), or (outside Entire Library) when several songs failed and fewer arrived; a later sync without such failures makes them: Retry Failed (after a server-wide failure, the only way), or take the failing songs out of Sync if they failed on their own.

## What it touches

Only `Jellyfin/`, the playlist `.m3u` files it wrote and `.jellyfin/` (songs an earlier version cached are in `.jellyfin/cache/`). It also reads Q2 Pod's play log (`.scrobbler.log` and `.scrobbler.log.sent`) to report plays, and never changes it; its `q2app` names `Jellyfin/` (`favdir=Jellyfin`) so Q2 Pod tells it of hearts given to synced songs. Nothing else on the card is read or changed: other apps' folders (`Subsonic/`, `Deezer/`, `Navidrome/`) and their playlists are left alone.

## Q2 Pod 1.7.3 or later

Jellyfin needs Q2 Pod 1.7.3 or later. On an older Q2 Pod it opens to one page that says *Needs Q2 Pod 1.7.3 or later. Update Q2 Pod to use Jellyfin.* and does nothing else: no sign-in, browsing, sync or streaming. Update Q2 Pod, then open Jellyfin again; what is on the card stays, and a `Jellyfin sign-in.txt` is read then.

After a sync the Q2 updates its library and imports the playlists itself. A playlist you made on the Q2 is never replaced: a server playlist with the same name arrives as "<name> (Jellyfin)". If the Q2 is switched off before Jellyfin has read the Q2's answer, Jellyfin asks again when next opened: the Q2 updates its library again (Back cancels it), and a playlist added in that sync can come a second time as "<name> (Jellyfin)".

## Known limitations

- **Smart Wi-Fi keeps Wi-Fi on for a queue played from Jellyfin.** For Q2 Pod, a queue played from Jellyfin is a stream, synced songs included, so Smart Wi-Fi keeps Wi-Fi on while it is the Q2's queue, playing, paused or stopped. Synced songs played from the Q2's own **Library** don't keep it on.

- **An account without the download permission can't sync lossy songs as they are.** Under an Opus Sync Quality, MP3, AAC and Opus songs sync as the server's file, which the server sends only to a user with **Allow media downloading**. Without it the sync pauses with *Downloads are turned off for this account on the server* and, in the row below, *Turn on Always Re-encode in Quality and Storage*. Either give the user the permission, or turn on **Always Re-encode** so those songs are transcoded like the rest. Under Original, which downloads every song, the row below says *Or choose an Opus Sync Quality and Always Re-encode*.
- **A streamed queue can need Jellyfin opened again.** Each song and cover of the queue Q2 Pod plays comes through the app, so Jellyfin stays running while the queue streams, song after song. Paused, stopped or stalled for 30 minutes (and no press), Jellyfin closes, as it always has: the queue then doesn't play on, and neither does one resumed after Jellyfin was closed (*Stop Streaming Services*, Rockbox), until Jellyfin is opened again. A Q2 Pod before 1.8.1 (without `now`) can't tell Jellyfin what plays, so there Jellyfin also closes 30 minutes after the last song it streamed: a queue whose synced songs play longer than that stops at the next streamed song until Jellyfin is opened again. A Q2 Pod change that starts Jellyfin when one of its songs starts is planned.
- **A cover can be missing for one draw after very heavy browsing.** Past about 1,500 covers kept, tidying the cover cache can remove a cover just before it's drawn; it shows again on the next draw.

## Errors and logs

A failure shows a short message on screen. Open its **Details** row for the operation, the HTTP status and the time. The full trail is in `.jellyfin/log.txt`; passwords and tokens are never logged. **Settings** shows the app's version, for a bug report.

## Update and remove

To remove it, press **Jellyfin** twice in **System settings → App Store**, or delete `.jellyfin/`. Synced music stays in `Jellyfin/`. Removing it doesn't end its sign-in on your server, so sign out first (**Settings → Account → Sign Out**). If it's already removed, revoke the Q2 under **Dashboard → Devices**. A Q2 still signed in with an API key isn't signed out by Sign Out either: delete the key under **Dashboard → API Keys**.

After a reinstall (`.jellyfin/` gone), the app no longer knows the files it put on the card. Before you sync again, delete `Jellyfin/` and the app's old playlist files: the `<name>.m3u` files in `_explaylist_data/` named after your Jellyfin playlists. If you don't, the playlists come back as "<name> (Jellyfin)", and old songs you don't sync again stay on the card.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.jellyfin` and the release ZIP beside them. The code is Linux-only (it runs on the Q2): on Windows, build and vet with `GOOS=linux`, and run the tests under Linux or WSL. More: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). Forked from [q2-subsonic](https://github.com/andsuor/q2-subsonic), itself built from [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser); see [NOTICE](NOTICE). Jellyfin is a trademark of the Jellyfin project; this app is not affiliated with it.
