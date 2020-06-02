![charo logo](./logo.png)

# charo-torrent

[![GoDoc](https://godoc.org/github.com/lkslts64/charo-torrent/torrent?status.svg)](https://godoc.org/github.com/lkslts64/charo-torrent/torrent) [![Join the chat at https://gitter.im/lkslts64/charo-torrent](https://badges.gitter.im/lkslts64/charo-torrent.svg)](https://gitter.im/lkslts64/charo-torrent?utm_source=badge&utm_medium=badge&utm_campaign=pr-badge&utm_content=badge)

This repository implements the BitTorrent protocol and comes with a minimal CLI BitTorrent
client. The [torrent package](https://godoc.org/github.com/lkslts64/charo-torrent/torrent) is written with focus on scalability, it is well documented and can be used by other projects as well. Interfaces like [PieceSelector](https://godoc.org/github.com/lkslts64/charo-torrent/torrent#PieceSelector) make this package configurable and distinct from others. Aside from the [core protocol specification](https://www.bittorrent.org/beps/bep_0003.html), charo implements:

* [Extension for Peers to Send Metadata Files (aka Magnet Links)](https://www.bittorrent.org/beps/bep_0009.html)
* [UDP Tracker Protocol](https://www.bittorrent.org/beps/bep_0015.html)
* [DHT Protocol](https://www.bittorrent.org/beps/bep_0005.html) ([anacrolix package](https://github.com/anacrolix/dht))
* [Tracker Scrape Extension](https://www.bittorrent.org/beps/bep_0048.html)
* [Tracker Returns Compact Peer Lists](https://www.bittorrent.org/beps/bep_0023.html)

As a side note, charo doesn't support IPv6 yet.

## Install

Go >= 1.13 is required

1. Library: `go get github.com/lkslts64/charo-torrent/torrent`
2. Client: `go get github.com/lkslts64/charo-torrent/cmd/charo-download`

## Client Usage

To download a torrent from a file:

    The following command assumes that the client is installed and the executable 'charo-download' is in $PATH (because $GOPATH/bin should be in $PATH). <file> is propably a file with .torrent extension.
    $ charo-download -torrentfile <file>
    The downloaded files will be available under the current working directory.

To download a torrent from a magnet:

    <magnet> is a magnet URI
    $ charo-download -magnet <magnet>

## Library Usage

Read the [docs](https://godoc.org/github.com/lkslts64/charo-torrent/torrent) or check the [minimal cli client](cmd/charo-download/main.go).

## Other notable Go torrent packages

* [torrent](https://github.com/anacrolix/torrent/)
* [rain](https://github.com/cenkalti/rain)

## Contribute

Contributions are welcome! Come to chat on gitter

## Credits

Logo designed by [Leia]()
