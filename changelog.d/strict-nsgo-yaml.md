### Changed

- `nsgo.yaml` refuses a key it does not know, at any depth, naming its line, its section and the nearest known key (`nsgo.yaml:12: android has no key permisions; did you mean permissions?`), where it used to ignore it and build the app without what the key asked for (#2475 by @houko).
