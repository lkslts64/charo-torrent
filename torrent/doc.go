/*
Package torrent implements the BitTorrent protocol.
It is designed to be simple and easy to use. A common workflow is to create a Client,
add a torrent and then download it.

	cl, _ := torrent.NewClient(nil)
	t, _ := cl.AddFromFile("example.torrent")
	<-t.InfoC
	t.StartDataTransfer()
	<-t.DownloadedDataC
	fmt.Println("torrent downloaded!")
*/
package torrent
