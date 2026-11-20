<p align="center"><img src="docs/hero.jpg" alt="A smoked black Miyoo Mini Plus on a stand showing server stats in MiniRack, on a worn workbench in an electrical room, with a multimeter, a circuit board, pliers and a grey one lying beside it" width="100%"></p>

[![CI](https://github.com/zakijss/MiniRack/actions/workflows/ci.yml/badge.svg)](https://github.com/zakijss/MiniRack/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/zakijss/MiniRack)](https://github.com/zakijss/MiniRack/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

# MiniRack

Live CPU, memory, disk, temperature, load and uptime for your servers, on a Miyoo Mini Plus. Part of [MiniDeck](https://github.com/zakijss/MiniDeck).

- Sources: [Glances](https://github.com/nicolargo/glances) (one machine), a [Beszel](https://beszel.dev) hub (every machine it watches) and [Vantage](https://github.com/zakijss/Vantage).
- A card per machine, and a detail page with every disk, sensor and service, and why a machine is down.
- Temperature and load turn orange from 75 °C or a load of 1 per thread, and red from 90 °C or 2 per thread.
- Servers on your private network (Tailscale, Headscale, WireGuard or NetBird) through [MiniScale](https://github.com/zakijss/MiniScale), or on your home network directly.

<p align="center"><img src="docs/desk.jpg" alt="From above: a white Miyoo Mini Plus showing MiniRack's settings and a smoked black one showing one server, on a workbench with a circuit board and pliers" width="100%"></p>

## Install

Open [MiniMart](https://github.com/zakijss/MiniMart) and pick **Apps > MiniRack**: it checks the download and keeps your settings on updates. Or unzip `minirack-VERSION.zip` from [Releases](https://github.com/zakijss/MiniRack/releases/latest) onto the root of the SD card and restart. Every MiniDeck app comes in one zip too: [minideck-all.zip](https://github.com/zakijss/MiniDeck/releases/latest/download/minideck-all.zip).

Needs a Miyoo Mini Plus on Onion OS 4.4, with Wi-Fi, and a server running Glances (`glances -w`), Beszel or Vantage. Other devices and firmwares aren't supported.

## Using it

The first start asks for a source: type the address or pick a device from your tailnet; **Save** tests the connection first. Use a read-only Beszel user.

- On the overview, **A** opens a machine, **L1**/**R1** move four machines at a time, **X** refreshes and **SELECT** changes the look.
- On a machine's page, **L1**/**R1** step to the other machines, **Up**/**Down** scroll and **X** refreshes.
- **Y** opens the sources and Settings: the refresh interval, °C or °F, the order of the machines (name, CPU, memory, load or status) and hiding machines that are down.

Passwords are kept on the card only encrypted, with the device key.

## Building

```sh
make check                     # vet, tests, scripts
make package VERSION=1.0.0     # dist/: the release zip, app.json, SHA256SUMS
make deploy HOST=root@miyoo    # build and copy to a device over SSH
```

Go runs in Docker unless you pass `GO=go` (Go 1.27). The interface, themes and networking come from the [MiniDeck library](https://github.com/zakijss/MiniDeck); see its [development notes](https://github.com/zakijss/MiniDeck/blob/main/docs/DEVELOPMENT.md) and [contributing guide](https://github.com/zakijss/MiniDeck/blob/main/CONTRIBUTING.md).

## License

[MIT](LICENSE). Third-party components are listed in [NOTICE](NOTICE). Not made by or affiliated with Miyoo.
