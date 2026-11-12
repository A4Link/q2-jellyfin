#!/usr/bin/env python3
"""Start a local Jellyfin test server for q2-jellyfin's integration tests, and fill it.

    python3 contrib/testserver/setup.py DATA_DIR            # create (or reuse) and fill
    python3 contrib/testserver/setup.py DATA_DIR --reset    # remove the container and data first

DATA_DIR (outside the repository) holds the server's config, cache and media, and server.json:
the URL, users, passwords, API key and item ids the tests read (JELLYFIN_TEST_SERVER=DATA_DIR/server.json).

Everything is synthetic: short sine tones made by the container's own ffmpeg, tagged as fake
albums. The server listens on 127.0.0.1 only. Decoys test that the app serves music only: a movie
(Movies library), an audiobook (Books library) and an MP3 in a mixed-content library, each also
favourited and put in playlists.

Needs Docker and Python 3 (standard library only).
"""
import json, os, shutil, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

IMAGE = "jellyfin/jellyfin:latest"  # recorded by digest and version in server.json
NAME = "q2-jellyfin-test"
PORT = 8096
URL = f"http://127.0.0.1:{PORT}"
FF = "/usr/lib/jellyfin-ffmpeg/ffmpeg"
AUTH = 'MediaBrowser Client="q2-jellyfin-testserver", Device="setup", DeviceId="q2-testserver-setup", Version="1"'

ADMIN, ADMIN_PW = "admin", "admin-pass"
USER, USER_PW = "q2", "q2-pass"
NODL, NODL_PW = "nodl", "nodl-pass"

# (folder under /media, file, codec args, tags, seconds)
TRACKS = [
    ("music/Test Artist/2001 - First Album", "01 - Alpha.flac", ["-c:a", "flac"],
     dict(title="Alpha", artist="Test Artist", album_artist="Test Artist", album="First Album", track="1/3", date="2001", genre="Test"), 6),
    ("music/Test Artist/2001 - First Album", "02 - Beta.flac", ["-c:a", "flac"],
     dict(title="Beta", artist="Test Artist", album_artist="Test Artist", album="First Album", track="2/3", date="2001", genre="Test"), 7),
    ("music/Test Artist/2001 - First Album", "03 - Gamma.flac", ["-c:a", "flac"],
     dict(title="Gamma", artist="Test Artist", album_artist="Test Artist", album="First Album", track="3/3", date="2001", genre="Test"), 8),
    ("music/Second Artist/2010 - Second Album", "01 - Delta.mp3", ["-c:a", "libmp3lame", "-b:a", "192k", "-id3v2_version", "3"],
     dict(title="Delta", artist="Second Artist", album_artist="Second Artist", album="Second Album", track="1/2", date="2010", genre="Test"), 6),
    ("music/Second Artist/2010 - Second Album", "02 - Epsilon.mp3", ["-c:a", "libmp3lame", "-b:a", "192k", "-id3v2_version", "3"],
     dict(title="Epsilon", artist="Second Artist", album_artist="Second Artist", album="Second Album", track="2/2", date="2010", genre="Test"), 6),
    ("music/Various Artists/2015 - Compilation", "01 - Zeta.opus", ["-c:a", "libopus", "-b:a", "96k"],
     dict(title="Zeta", artist="Guest One", album_artist="Various Artists", album="Compilation", track="1/2", date="2015", genre="Test"), 6),
    ("music/Various Artists/2015 - Compilation", "02 - Eta.m4a", ["-c:a", "aac", "-b:a", "128k"],
     dict(title="Eta", artist="Guest Two", album_artist="Various Artists", album="Compilation", track="2/2", date="2015", genre="Test"), 6),
    ("music/Odd Artist/2020 - Odd Album", "01 - Theta.wv", ["-c:a", "wavpack"],
     dict(title="Theta", artist="Odd Artist", album_artist="Odd Artist", album="Odd Album", track="1/1", date="2020", genre="Test"), 6),
    # decoys: not music libraries
    ("audiobooks/Book Author/Test Book", "Test Book.m4b", ["-c:a", "aac", "-b:a", "64k", "-f", "mp4"],
     dict(title="Test Book", artist="Book Author", album_artist="Book Author", album="Test Book", genre="Audiobook"), 8),
    ("mixed", "Voice Memo.mp3", ["-c:a", "libmp3lame", "-b:a", "128k", "-id3v2_version", "3"],
     dict(title="Voice Memo", artist="Memo Person", album="Memos", genre="Memo"), 5),
]
MOVIE = ("movies/Test Movie (2020)", "Test Movie (2020).mp4")
# synced lyrics beside Alpha, as a .lrc: the server reads it, and the app writes it to the card
LYRICS = ("music/Test Artist/2001 - First Album", "01 - Alpha.lrc",
          "[ar:Test Artist]\n[ti:Alpha]\n[00:00.50]First line of Alpha\n[00:02.25]Second line of Alpha\n"
          "[00:04.00]Third line of Alpha\n")
LIBRARIES = [("Music", "music", "/media/music"), ("Movies", "movies", "/media/movies"),
             ("Audiobooks", "books", "/media/audiobooks"), ("Mixed", None, "/media/mixed")]


def sh(*args, check=True, quiet=False):
    r = subprocess.run(list(args), capture_output=True, text=True)
    if check and r.returncode != 0:
        sys.exit(f"{' '.join(args)}\n{r.stderr.strip()}")
    if not quiet and r.stdout.strip():
        print(r.stdout.strip())
    return r


def api(method, path, body=None, token=None, query=None):
    q = ("?" + urllib.parse.urlencode(query, doseq=True)) if query else ""
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(URL + path + q, data=data, method=method)
    req.add_header("Authorization", AUTH + (f', Token="{token}"' if token else ""))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read()
            return json.loads(raw) if raw.strip() else None
    except urllib.error.HTTPError as e:
        sys.exit(f"{method} {path}: HTTP {e.code} {e.read()[:300]!r}")


def wait_up():
    for _ in range(120):
        try:
            with urllib.request.urlopen(URL + "/System/Info/Public", timeout=5) as r:
                if r.status == 200:
                    return json.load(r)
        except Exception:
            pass
        time.sleep(2)
    sys.exit("the server didn't come up")


def make_media(data):
    media = os.path.join(data, "media")
    for folder, name, codec, tags, secs in TRACKS:
        if os.path.exists(os.path.join(media, folder, name)):
            continue
        sh("docker", "exec", NAME, "mkdir", "-p", f"/media/{folder}")
        meta = []
        for k, v in tags.items():
            meta += ["-metadata", f"{k}={v}"]
        freq = 220 + 40 * len(name)
        sh("docker", "exec", NAME, FF, "-v", "error", "-y", "-f", "lavfi", "-i", f"sine=frequency={freq}:duration={secs}",
           *codec, *meta, f"/media/{folder}/{name}")
    folder, name, text = LYRICS
    if not os.path.exists(os.path.join(media, folder, name)):
        with open(os.path.join(media, folder, name), "w", encoding="utf-8", newline="\n") as f:
            f.write(text)
    folder, name = MOVIE
    if not os.path.exists(os.path.join(media, folder, name)):
        sh("docker", "exec", NAME, "mkdir", "-p", f"/media/{folder}")
        sh("docker", "exec", NAME, FF, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=duration=6:size=320x240:rate=10",
           "-f", "lavfi", "-i", "sine=frequency=330:duration=6", "-c:v", "libx264", "-c:a", "aac", "-shortest",
           "-metadata", "title=Test Movie", f"/media/{folder}/{name}")


def indexed(token, uid):
    """Every expected file, by its /media path -> its item (id, name, album), once the server has
    read its tags; None while the scan or the metadata reads are still running."""
    r = api("GET", "/Items", token=token, query={"userId": uid, "recursive": "true", "fields": "Path",
                                                 "includeItemTypes": "Audio,AudioBook,Movie"})
    by_path = {it.get("Path"): it for it in r["Items"]}
    out = {}
    for folder, name, _, tags, _ in TRACKS + [(MOVIE[0], MOVIE[1], None, {}, 0)]:
        it = by_path.get(f"/media/{folder}/{name}")
        if not it or (tags.get("album") and it.get("Type") == "Audio" and it.get("Album") != tags["album"]):
            return None
        out[tags.get("title", "Test Movie")] = it["Id"]
    return out


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    data = os.path.abspath(sys.argv[1])
    if "--reset" in sys.argv:
        sh("docker", "rm", "-f", NAME, check=False, quiet=True)
        if os.path.isdir(data) and os.path.exists(os.path.join(data, "server.json")):
            shutil.rmtree(data)
    for d in ("config", "cache", "media"):
        os.makedirs(os.path.join(data, d), exist_ok=True)

    running = sh("docker", "ps", "-q", "-f", f"name=^{NAME}$", quiet=True).stdout.strip()
    if not running:
        sh("docker", "rm", "-f", NAME, check=False, quiet=True)
        sh("docker", "run", "-d", "--name", NAME, "-p", f"127.0.0.1:{PORT}:8096",
           "-v", f"{os.path.join(data, 'config')}:/config", "-v", f"{os.path.join(data, 'cache')}:/cache",
           "-v", f"{os.path.join(data, 'media')}:/media", IMAGE, quiet=True)
    info = wait_up()
    print("server", info.get("Version"), info.get("ServerName"))
    make_media(data)

    if not info.get("StartupWizardCompleted"):
        api("POST", "/Startup/Configuration", {"UICulture": "en-US", "MetadataCountryCode": "GB", "PreferredMetadataLanguage": "en"})
        api("GET", "/Startup/User")
        api("POST", "/Startup/User", {"Name": ADMIN, "Password": ADMIN_PW})
        api("POST", "/Startup/RemoteAccess", {"EnableRemoteAccess": True})
        api("POST", "/Startup/Complete")
    auth = api("POST", "/Users/AuthenticateByName", {"Username": ADMIN, "Pw": ADMIN_PW})
    tok, admin_id = auth["AccessToken"], auth["User"]["Id"]

    have = {f["Name"] for f in api("GET", "/Library/VirtualFolders", token=tok)}
    for name, ctype, path in LIBRARIES:
        if name not in have:
            q = {"name": name, "paths": path, "refreshLibrary": "false"}
            if ctype:
                q["collectionType"] = ctype
            api("POST", "/Library/VirtualFolders", {"LibraryOptions": {}}, tok, q)
    api("POST", "/Library/Refresh", token=tok)

    cfg = api("GET", "/System/Configuration", token=tok)
    if not cfg.get("QuickConnectAvailable"):
        cfg["QuickConnectAvailable"] = True
        api("POST", "/System/Configuration", cfg, tok)

    users = {u["Name"]: u for u in api("GET", "/Users", token=tok)}
    ids = {}
    for name, pw, download in ((USER, USER_PW, True), (NODL, NODL_PW, False)):
        u = users.get(name) or api("POST", "/Users/New", {"Name": name, "Password": pw}, tok)
        pol = api("GET", f"/Users/{u['Id']}", token=tok)["Policy"]
        pol["EnableContentDownloading"] = download
        api("POST", f"/Users/{u['Id']}/Policy", pol, tok)
        ids[name] = u["Id"]
    uid = ids[USER]

    # wait for the scan and the tag reads: every file indexed, each song with its album
    for _ in range(120):
        item = indexed(tok, uid)
        if item:
            break
        time.sleep(2)
    else:
        sys.exit("the scan didn't finish in 4 minutes")
    for _ in range(60):  # Alpha's lyrics read
        if api("GET", f"/Items/{item['Alpha']}", token=tok, query={"userId": uid}).get("HasLyrics"):
            break
        api("POST", "/Library/Refresh", token=tok)
        time.sleep(3)
    else:
        sys.exit("Alpha's lyrics weren't read")
    for t in ("Alpha", "Delta", "Zeta", "Voice Memo", "Test Movie", "Test Book"):  # the last three are decoys
        api("POST", f"/UserFavoriteItems/{item[t]}", token=tok, query={"userId": uid})

    playlists = {}
    existing = api("GET", "/Items", token=tok, query={"userId": uid, "recursive": "true", "includeItemTypes": "Playlist"})["Items"]
    by_name = {p["Name"]: p["Id"] for p in existing}
    for name, mtype, members in (("Road Trip", "Audio", ["Beta", "Alpha", "Beta", "Eta"]),
                                 ("Mixed Bag", "Audio", ["Gamma", "Voice Memo", "Test Book", "Epsilon"]),
                                 ("Movie Night", "Video", ["Test Movie"])):
        if name not in by_name:
            r = api("POST", "/Playlists", {"Name": name, "Ids": [item[m] for m in members], "UserId": uid, "MediaType": mtype}, tok)
            by_name[name] = r["Id"]
        playlists[name] = by_name[name]

    keys = api("GET", "/Auth/Keys", token=tok)["Items"]
    key = next((k["AccessToken"] for k in keys if k.get("AppName") == "q2-jellyfin-test"), None)
    if not key:
        api("POST", "/Auth/Keys", token=tok, query={"app": "q2-jellyfin-test"})
        key = next(k["AccessToken"] for k in api("GET", "/Auth/Keys", token=tok)["Items"] if k.get("AppName") == "q2-jellyfin-test")

    digest = sh("docker", "image", "inspect", IMAGE, "--format", "{{index .RepoDigests 0}}", quiet=True).stdout.strip()
    out = {"url": URL, "version": info.get("Version"), "image": digest, "admin": {"name": ADMIN, "password": ADMIN_PW, "id": admin_id},
           "user": {"name": USER, "password": USER_PW, "id": uid}, "nodl": {"name": NODL, "password": NODL_PW, "id": ids[NODL]},
           "apiKey": key, "items": item, "playlists": playlists,
           "favourites": ["Alpha", "Delta", "Zeta", "Voice Memo", "Test Movie", "Test Book"]}
    with open(os.path.join(data, "server.json"), "w", encoding="utf-8") as f:
        json.dump(out, f, indent=2)
    print("ready:", os.path.join(data, "server.json"))


if __name__ == "__main__":
    main()
