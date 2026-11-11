# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- Settings › Sort machines has Load: the busiest machine first, by its 1-minute load.
- A hot or overloaded machine shows its temperature and load in orange or red, on its card and on its page. Orange from 75 °C or a load of 1 per CPU thread, red from 90 °C or 2.
- When the sources list no machine, the overview says so and that X reads them again, instead of "Reading your servers…".

### Changed

- A machine's page shows X Refresh in the footer.
- The selected machine stays selected after Settings.
- Go 1.27.4 and updated dependencies.

## [1.0.3] - 2026-11-08

### Changed

- An unreadable settings file says to fix or delete settings.json, then open MiniRack again.
- A wrong Glances address says to check that the server runs glances -w.
- A failed save says to check that the card has free space.
- "No recent data" starts with a capital.
- Grey and red small text is easier to read (MiniDeck 1.0.3).

## [1.0.2] - 2026-11-03

### Fixed

- A very long server uptime no longer shows as a wrong or negative number on the device.

## [1.0.1] - 2026-11-02

### Fixed

- The on-screen keyboard now shows how to leave it: B on an empty field goes back, and the footer shows B Back, or MENU Cancel once there is text (MiniDeck 1.0.1).

## [1.0.0] - 2026-11-02

### Added

- The first release: Glances, Beszel and Vantage sources, server cards and a detail page per machine, and settings for °C or °F, sorting the machines and hiding the ones that are down.

[Unreleased]: https://github.com/zakijss/MiniRack/compare/v1.0.3...HEAD
[1.0.3]: https://github.com/zakijss/MiniRack/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniRack/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniRack/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniRack/releases/tag/v1.0.0
