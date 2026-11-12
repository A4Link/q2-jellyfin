# Jellyfin test server

A local Jellyfin in Docker, filled with synthetic music, for q2-jellyfin's integration and
end-to-end tests.

    python3 contrib/testserver/setup.py ../jellyfin-test            # start (or reuse) and fill
    python3 contrib/testserver/setup.py ../jellyfin-test --reset    # start again from nothing

It needs Docker and Python 3. The data folder (here `../jellyfin-test`, outside the repository)
holds the server's config, cache and media, and `server.json`: the URL, test users, passwords, API
key and item ids. The server listens on **127.0.0.1:8096 only**.

What it makes:

| | |
|---|---|
| Music library | 4 albums, 8 songs: FLAC, MP3, Opus, AAC (.m4a) and WavPack, tagged; short sine tones |
| Decoys | a movie (Movies), an audiobook (Audiobooks), an MP3 in a mixed-content library (Mixed) |
| Users | `admin` / `admin-pass`; `q2` / `q2-pass` (may download); `nodl` / `nodl-pass` (may not) |
| For `q2` | favourites Alpha, Delta, Zeta and the three decoys; playlists Road Trip (Beta, Alpha, Beta, Eta), Mixed Bag (Gamma, the mixed MP3, the audiobook, Epsilon) and Movie Night (the movie) |
| Server | Quick Connect on; an API key named `q2-jellyfin-test` |

Run the tests against it (the app's own tests need Linux, e.g. WSL):

    JELLYFIN_TEST_SERVER=../jellyfin-test/server.json go test ./internal/jellyfin -run Live -v
    JELLYFIN_TEST_SERVER=../jellyfin-test/server.json go test ./cmd/q2-jellyfin -run Live -v   # Linux

Stop it with `docker stop q2-jellyfin-test`; `docker start q2-jellyfin-test` brings it back with
everything in place. The image is `jellyfin/jellyfin:latest`; `server.json` records the digest and
version it was made with (12.4.0 when written).
