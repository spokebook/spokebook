# Spokebook

A self-hosted web app for documenting the maintenance of your bikes: components with purchase data and
mileage, a catalog of maintenance steps ordered by phase and assembly group, torques and wear limits,
and a history of the maintenance you have done.

**Website: [spokebook.org](https://spokebook.org)** with features, installation guide, documentation
and news, in English and German.

> **Status: early development.** Expect missing features and breaking changes; back up the database.

> **Spokebook only makes suggestions.** You are responsible for the maintenance of your bike; the
> manufacturer's instructions always take precedence. See the [disclaimer](#disclaimer).

![Start page with three bikes, each showing its mileage, the next maintenance due and the last one done](docs/bikes-desktop-dark.jpg)

## Why

Existing apps mostly count kilometers from Strava and send reminders. Spokebook puts the maintenance
itself first: you pick a level for a bike (pre-ride check, monthly check, minor or major service), work
through its steps from the catalog and record measurements along the way, such as chain stretch or
brake pad thickness. Features of a bike such as a carbon frame, tubeless tires, an e-bike drive,
suspension, hydraulic brakes, a hub gear, a belt drive or electronic shifting add, replace or drop
steps and add hints. Values beyond their limit are flagged. The steps are copied when you start, so the
history stays correct even if you change the catalog later.

## Roadmap

- **v0.1**: login with passkeys, bikes, mileage readings, components and their installations,
  a step catalog with phases, assembly groups and levels including a built-in starter catalog,
  performing and checking off a maintenance
- **v0.2**: due dates by mileage and time, attachments (invoices, photos), installable as a PWA,
  mileage import from GPX/FIT/TCX files and Garmin Connect
- **v0.3**: more modules switched on by bike features (hydraulic brakes, hub gear, belt drive,
  electronic shifting) and smaller improvements
- **v0.4**: optional serial numbers for bikes (frame number) and components
- **v0.5**: notifications over interchangeable channels (first ntfy) about due maintenance, worn
  components, Garmin errors, rides without bike and forgotten open maintenances
- **Later**: own modules in the UI, OIDC, CSV export, inviting more users, a public API, sharing
  components you entered (type, values such as wear limits and torques) with other users

The current state is on the [roadmap page](https://spokebook.org/en/roadmap/) of the website, details
are in the [concept](docs/konzept.md) (German).

## Technology

Go with `net/http` from the standard library, server-rendered HTML with
[htmx](https://htmx.org) (0BSD) and [Pico CSS](https://picocss.com) (MIT), both shipped in
`web/static/vendor`, SQLite. The app is a
single binary with embedded templates and migrations and needs no CGO.

## Running with Docker

Every release is published as a container image for `linux/amd64` on the GitHub Container Registry:
`ghcr.io/spokebook/spokebook` with the tags `<version>` (e.g. `0.3.0`), `<major>.<minor>` and `latest`.
The image listens on port 8080 and keeps its database in `/data`; it runs as user 65532 by default.
See also the [installation guide](https://spokebook.org/en/install/) on the website.

```yaml
services:
  spokebook:
    image: ghcr.io/spokebook/spokebook:latest
    restart: unless-stopped
    user: "568:568"            # optional, e.g. the TrueNAS "apps" user; must own ./data
    ports:
      - "8080:8080"
    environment:
      APP_BASE_URL: https://bikes.example.com
      APP_TRUSTED_PROXIES: 192.168.1.10   # address of the reverse proxy as the container sees it
      # APP_SECRET_KEY: ...    # needed for Garmin Connect and notifications; create one with: openssl rand -base64 32
      # APP_NOTIFY_ALLOW_PRIVATE: "true"   # only for an ntfy server in your own network
    volumes:
      - ./data:/data
```

`APP_BASE_URL` must be the exact address users open, since passkeys and `Secure` cookies depend on it.
Behind a reverse proxy that terminates TLS, set it to the `https://` address and let the proxy pass
the original `Host` header through (Traefik does by default); requests whose `Origin` does not match
the host are rejected as cross-site. The setup link for the first account appears in the container
log.

To build the image yourself: `docker build -t spokebook .`

### Configuration

All settings are environment variables. The defaults suit development; in the image, `APP_ADDR` is
`:8080` and `APP_DB_PATH` is `/data/spokebook.sqlite`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `APP_ADDR` | `localhost:8080` | Address the server listens on |
| `APP_DB_PATH` | `data/dev.sqlite` | Path to the SQLite database; created and migrated at startup |
| `APP_BASE_URL` | `http://localhost:8080` | Address users open. Passkeys are bound to its host; with `https://`, cookies are `Secure` and HSTS is sent |
| `APP_TRUSTED_PROXIES` | (none) | Comma-separated IP addresses or CIDR ranges of reverse proxies whose `X-Forwarded-For` header is believed, e.g. `192.168.1.10` or `172.16.0.0/12` |
| `APP_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; `debug` logs every request with method, path, status, duration and client address |
| `APP_SECRET_KEY` | (none) | At least 32 characters, e.g. from `openssl rand -base64 32`. Encrypts the credentials of ride sources such as the Garmin API key and the settings of notification channels; without it, Garmin Connect can't be connected and notifications are off. If it changes, the stored keys and channel settings must be entered again |
| `APP_NOTIFY_ALLOW_PRIVATE` | `false` | `true` lets notification channels reach servers at loopback and private addresses (`127.0.0.0/8`, `::1`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, `fc00::/7`, local NAT64 `64:ff9b:1::/48`), e.g. a self-hosted ntfy server in your home network. Link-local, site-local, multicast, broadcast and unspecified addresses, `192.0.0.0/24` `0.0.0.0/8`, the cloud metadata addresses (`169.254.169.254`, `fd00:ec2::254`, `fd20:ce::254`, `fd00:c1::a9fe:a9fe`, `100.100.100.200`, `168.63.129.16`) and the IPv6 ranges that tunnel or translate to IPv4 (6to4 `2002::/16`, Teredo `2001::/32`, SIIT `::ffff:0:0:0/96`) are never allowed; NAT64 (`64:ff9b::/96`) and IPv4-compatible addresses are judged by their embedded IPv4 address. A NAT64 prefix chosen by your network operator is not recognized. Any other value than `true`/`false` (or `1`/`0`) stops the start |

### First login

As long as there is no account, the server writes a setup link with a one-time token to its log at
every start (`no account yet, open this link to create one`). Open it and create the account with a
name and password; afterwards you can add passkeys on the account page. Data entered before login
existed belongs to this first account.

Login is limited to 10 attempts per client and minute. After 5 wrong passwords in a row, the account
is locked for 15 minutes. Behind a reverse proxy, set `APP_TRUSTED_PROXIES` to the
proxy's address so the limit applies per client; otherwise all clients share the proxy's address and
the limit applies to all of them together.

### Backup

Under *Account → Backup*, **Export** downloads all your data (bikes, odometer readings, components,
component types, catalog, intervals, maintenances, rides and their bike mappings, photos and documents,
shopping list) as a ZIP file with a versioned JSON file and the attachments. **Import** restores such a
file: it replaces all of your data in one transaction, so a damaged file changes nothing. Accounts,
passwords, passkeys, ride source credentials (Garmin API key) and notification channels are not part
of the backup and stay as they are; the notification settings (events, threshold, days, language) are
included. A backup from an older version of Spokebook can be imported into a newer one, including the
plain JSON files of the first backup format.

To back up the whole server including accounts, copy the database in `/data` while the container is
stopped.

### Garmin Connect

Under *Rides → Garmin Connect*, paste a personal API key created in Garmin Connect (*Settings →
Personal API Key*) and choose the first day to fetch. Spokebook then fetches your cycling activities
once a day (or with *Fetch now*), downloads the original FIT file of each new one and imports it like
an uploaded file; the Garmin gear chosen for a ride decides the bike. To be gentle with Garmin's rate
limit, requests are at least 2 seconds apart and a run downloads at most 100 activities; a longer
history continues hourly until it is complete, then fetching is daily again. The key is stored encrypted with `APP_SECRET_KEY` and is not part of the
backup. Garmin keys expire after 30 days to one year; the page tells you when a new one is needed.

### Notifications

Under *Account → Notifications*, add a channel to be told when something needs doing; it needs
`APP_SECRET_KEY`, which encrypts the channel settings. The first kind of channel is
[ntfy](https://ntfy.sh): enter the server (default `https://ntfy.sh`), a topic and, for a protected
topic, an access token, then subscribe to the topic in the ntfy app. On a public server anyone who
knows the topic can read the messages, so choose a long, random one. A self-hosted ntfy server in your
own network (a loopback or private address) is only reached if the server runs with
`APP_NOTIFY_ALLOW_PRIVATE=true`. *Test* sends a test message; the last error is shown at the channel.

Once a day Spokebook checks your data and notifies you about the chosen events: due maintenance,
worn components (from 80 % of the maximum distance; a replace-by date when it is reached), open
maintenances older than a number of days, rides without bike and a Garmin API key that needs
replacing. You choose whether "due soon" counts or only "overdue". Each state is notified once: again
only when it gets worse (soon to overdue, more rides without bike) or when it comes back after being
resolved. The items of a bike come in one message with a link to it.

## Development

The easiest way is the included dev container (`.devcontainer/`), which has Go, sqlc, goose, air and
golangci-lint installed.

```sh
air                      # run the app with live reload
go run ./cmd/spokebook   # or without live reload
go test ./...
```

Then open `http://localhost:8080`. Use `localhost`, not `127.0.0.1`, or passkeys won't work. Outside
Docker, the defaults in the table above apply.

## Contributing

Issues and pull requests are welcome; see also [Contribute](https://spokebook.org/en/contribute/) on
the website. After changing queries or migrations, run `sqlc generate` and commit the generated
files. Conventions and the directory layout are described in [CLAUDE.md](CLAUDE.md).

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org): `fix:` leads to
a patch release, `feat:` to a minor release, and a `BREAKING CHANGE:` footer to a major release.
Releases are made by [semantic-release](https://semantic-release.gitbook.io) on every push to `main`,
which also builds and pushes the image.
Dependabot proposes updates of the Go modules, base images and actions once a week.

Please report security problems privately, see [SECURITY.md](SECURITY.md).

## Disclaimer

You maintain and repair your bike at your own responsibility. A badly maintained bike – with bolts
tightened wrongly, worn brake pads or a damaged carbon part, for example – can cause serious
accidents.

The maintenance steps, their order, levels, intervals, hints, follow-up steps and guidance values in
the built-in catalog and in the documentation are non-binding suggestions. They are general, not
complete, and may be wrong for your bike and your components. A maintenance that Spokebook shows as
finished, or a measurement it shows as within its limit, does not confirm that your bike is safe to
ride. Spokebook only works with the values you enter.

The instructions and figures of the manufacturers – torques, wear limits, service intervals, assembly
compounds – always apply. If you are unsure about a job or lack the tools or the experience, have it
done by a professional bike shop, in particular for brakes, steering, wheels, frame and fork, and
e-bike batteries.

Spokebook is provided without warranty, to the extent permitted by law; see sections 15 and 16 of
the [license](LICENSE).

## License

Copyright (C) 2026 Michael Neuendorf

Spokebook is licensed under the GNU Affero General Public License v3.0, see [LICENSE](LICENSE). If
you run a modified version as a network service, you must make its source code available to its
users. The link in the footer of every page points to `SourceURL` in
`internal/app/spokebook/version.go`; set it to your copy.
