package dctoolkit

import (
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"
)

const (
	_PEER_WAIT_TIMEOUT = 10 * time.Second
)

// DownloadConf allows to configure a download.
type DownloadConf struct {
	// the peer from which downloading
	Peer *Peer
	// the TTH of the file to download
	TTH TigerHash
	// the starting point of the file part to download, in bytes
	Start uint64
	// the length of the file part. Leave zero to download the entire file
	Length int64
	// if filled, the file is saved on the desired path on disk, otherwise it is kept on RAM
	SavePath string
	// after download, do not attempt to validate the file through its TTH
	SkipValidation bool

	isFilelist bool
}

// Download represents an in-progress file download.
type Download struct {
	conf               DownloadConf
	client             *Client
	terminateRequested bool
	terminate          chan struct{}
	state              string
	activeDlChan       chan struct{}
	slotChan           chan struct{}
	peerChan           chan struct{}
	pconn              *connPeer
	query              string
	adcToken           string
	writer             io.WriteCloser
	content            []byte
	offset             uint64
	length             uint64
	lastPrintTime      time.Time
}

func (*Download) isTransfer() {}

// DownloadCount returns the number of remaining downloads, queued or active.
func (c *Client) DownloadCount() int {
	count := 0
	for t := range c.transfers {
		if _, ok := t.(*Download); ok {
			count++
		}
	}
	return count
}

func (c *Client) downloadByAdcToken(adcToken string) *Download {
	for t := range c.transfers {
		if dl, ok := t.(*Download); ok {
			if dl.adcToken == adcToken && dl.state == "waiting_peer" {
				return dl
			}
		}
	}
	return nil
}

func (c *Client) downloadPendingByPeer(peer *Peer) *Download {
	dl, ok := c.activeDownloadsByPeer[peer.Nick]
	if ok && dl.terminateRequested == false && dl.state == "waiting_peer" {
		return dl
	}
	return nil
}

// DownloadFileList starts downloading the file list of a given peer.
func (c *Client) DownloadFileList(peer *Peer, savePath string) (*Download, error) {
	return c.DownloadFile(DownloadConf{
		Peer:       peer,
		SavePath:   savePath,
		isFilelist: true,
	})
}

// DownloadFLFile starts downloading a file given a file list entry.
func (c *Client) DownloadFLFile(peer *Peer, file *FileListFile, savePath string) (*Download, error) {
	return c.DownloadFile(DownloadConf{
		Peer:     peer,
		TTH:      file.TTH,
		SavePath: savePath,
	})
}

// DownloadFLDirectory starts downloading recursively all the files
// inside a file list directory.
func (c *Client) DownloadFLDirectory(peer *Peer, dir *FileListDirectory, savePath string) error {
	var dlDir func(sdir *FileListDirectory, dpath string) error
	dlDir = func(sdir *FileListDirectory, dpath string) error {
		// create destionation directory if does not exist
		os.Mkdir(dpath, 0755)

		for _, file := range sdir.Files {
			_, err := c.DownloadFLFile(peer, file, filepath.Join(dpath, file.Name))
			if err != nil {
				return err
			}
		}
		for _, ssdir := range sdir.Dirs {
			err := dlDir(ssdir, filepath.Join(dpath, ssdir.Name))
			if err != nil {
				return err
			}
		}
		return nil
	}
	return dlDir(dir, savePath)
}

// DownloadFile starts downloading a file by its Tiger Tree Hash (TTH). See DownloadConf for the options.
func (c *Client) DownloadFile(conf DownloadConf) (*Download, error) {
	if conf.Length <= 0 {
		conf.Length = -1
	}

	d := &Download{
		conf:         conf,
		client:       c,
		terminate:    make(chan struct{}, 1),
		state:        "uninitialized",
		activeDlChan: make(chan struct{}),
		slotChan:     make(chan struct{}),
		peerChan:     make(chan struct{}),
	}
	d.client.transfers[d] = struct{}{}

	// build query
	d.query = func() string {
		if d.conf.isFilelist == true {
			return "file files.xml.bz2"
		}
		return "file TTH/" + d.conf.TTH.String()
	}()

	dolog(LevelInfo, "[download] [%s] request %s (s=%d l=%d)",
		d.conf.Peer.Nick, dcReadableQuery(d.query), d.conf.Start, d.conf.Length)

	d.client.wg.Add(1)
	go d.do()
	return d, nil
}

// Conf returns the configuration passed at download initialization.
func (d *Download) Conf() DownloadConf {
	return d.conf
}

// Content returns the downloaded file content ONLY if SavePath is not used, otherwise
// file content is saved directly on disk
func (d *Download) Content() []byte {
	return d.content
}

// Close stops the download. OnDownloadError and OnDownloadSuccessful are not called.
func (d *Download) Close() {
	if d.terminateRequested == true {
		return
	}
	d.terminateRequested = true

	if d.state != "processing" {
		d.terminate <- struct{}{}
	} else {
		d.pconn.close()
	}
}

func (d *Download) do() {
	defer d.client.wg.Done()

	err := func() error {
		// check if there are other downloads active on peer and eventually wait
		wait := false
		d.client.Safe(func() {
			if _, ok := d.client.activeDownloadsByPeer[d.conf.Peer.Nick]; ok {
				d.state = "waiting_activedl"
				wait = true
			} else {
				d.state = "waited_activedl"
				d.client.activeDownloadsByPeer[d.conf.Peer.Nick] = d
			}
		})
		if wait == true {
			select {
			case <-d.terminate:
				return errorTerminated
			case <-d.activeDlChan:
			}
		}

		// check if there is a download slot available and eventually wait
		wait = false
		d.client.Safe(func() {
			if d.client.downloadSlotAvail <= 0 {
				d.state = "waiting_slot"
				wait = true
			} else {
				d.state = "waited_slot"
				d.client.downloadSlotAvail -= 1
			}
		})
		if wait == true {
			select {
			case <-d.terminate:
				return errorTerminated
			case <-d.slotChan:
			}
		}

		// check if there is a connection with peer and eventually wait
		wait = false
		d.client.Safe(func() {
			if pconn, ok := d.client.connPeersByKey[nickDirectionPair{d.conf.Peer.Nick, "download"}]; !ok {
				dolog(LevelDebug, "[download] [%s] requesting new connection", d.conf.Peer.Nick)

				// generate new token
				if d.client.protoIsAdc == true {
					d.adcToken = adcRandomToken()
				}

				d.client.peerRequestConnection(d.conf.Peer, d.adcToken)
				d.state = "waiting_peer"
				wait = true

			} else {
				dolog(LevelDebug, "[download] [%s] using existing connection", d.conf.Peer.Nick)
				pconn.state = "delegated_download"
				pconn.transfer = d
				d.pconn = pconn
				d.state = "processing"
			}
		})
		if wait == true {
			timeout := time.NewTimer(_PEER_WAIT_TIMEOUT)
			select {
			case <-timeout.C:
				return fmt.Errorf("timed out")
			case <-d.terminate:
				return errorTerminated
			case <-d.peerChan:
			}
		}

		// process download
		dolog(LevelInfo, "[download] [%s] processing", d.conf.Peer.Nick)

		if d.client.protoIsAdc == true {
			d.pconn.conn.Write(&msgAdcCGetFile{
				msgAdcTypeC{},
				msgAdcKeyGetFile{
					Query:  d.query,
					Start:  d.conf.Start,
					Length: d.conf.Length,
					Compressed: (d.client.conf.PeerDisableCompression == false &&
						(d.conf.Length <= 0 || d.conf.Length >= (1024*10))),
				},
			})
		} else {
			d.pconn.conn.Write(&msgNmdcGetFile{
				Query:  d.query,
				Start:  d.conf.Start,
				Length: d.conf.Length,
				Compressed: (d.client.conf.PeerDisableCompression == false &&
					(d.conf.Length <= 0 || d.conf.Length >= (1024*10))),
			})
		}

		// exit this routine and do the work in the peer routine
		return nil
	}()

	if err != nil {
		d.client.Safe(func() {
			d.handleExit(err)
		})
	}
}

func (d *Download) handleSendFile(reqQuery string, reqStart uint64,
	reqLength uint64, reqCompressed bool) error {

	if reqQuery != d.query {
		return fmt.Errorf("filename returned by client is wrong: %s vs %s", reqQuery, d.query)
	}
	if reqStart != d.conf.Start {
		return fmt.Errorf("peer returned wrong start: %d instead of %d", reqStart, d.conf.Start)
	}
	if reqCompressed == true && d.client.conf.PeerDisableCompression == true {
		return fmt.Errorf("compression is active but is disabled")
	}

	if d.conf.Length == -1 {
		d.length = reqLength
	} else {
		d.length = uint64(d.conf.Length)
		if d.length != reqLength {
			return fmt.Errorf("peer returned wrong length: %d instead of %d", d.length, reqLength)
		}
	}

	if d.length == 0 {
		return fmt.Errorf("downloading null files is not supported")
	}

	d.pconn.conn.SetReadBinary(true)
	if reqCompressed == true {
		d.pconn.conn.ReaderEnableZlib()
	}

	// save in file
	if d.conf.SavePath != "" {
		f, err := os.Create(d.conf.SavePath + ".tmp")
		if err != nil {
			return fmt.Errorf("unable to create destination file")
		}
		d.writer = f

		// save in ram
	} else {
		d.content = make([]byte, d.length)
		d.writer = newBytesWriteCloser(d.content)
	}

	// setup time to correctly compute speed
	d.lastPrintTime = time.Now()

	return nil
}

func (d *Download) handleDownload(msgi msgDecodable) error {
	switch msg := msgi.(type) {
	case *msgAdcCStatus:
		return fmt.Errorf("error: %+v", msg)

	case *msgAdcCSendFile:
		return d.handleSendFile(msg.Query, msg.Start, msg.Length, msg.Compressed)

	case *msgNmdcMaxedOut:
		return fmt.Errorf("maxed out")

	case *msgNmdcError:
		return fmt.Errorf("error: %s", msg.Error)

	case *msgNmdcSendFile:
		return d.handleSendFile(msg.Query, msg.Start, msg.Length, msg.Compressed)

	case *msgBinary:
		newLength := d.offset + uint64(len(msg.Content))
		if newLength > d.length {
			return fmt.Errorf("binary content too long (%d)", newLength)
		}

		_, err := d.writer.Write(msg.Content)
		if err != nil {
			d.writer.Close()
			return err
		}
		d.offset = newLength

		since := time.Since(d.lastPrintTime)
		if since >= (1 * time.Second) {
			d.lastPrintTime = time.Now()
			speed := float64(d.pconn.conn.PullReadCounter()) / 1024 / (float64(since) / float64(time.Second))
			dolog(LevelInfo, "[recv] %d/%d (%.1f KiB/s)", d.offset, d.length, speed)
		}

		if d.offset == d.length {
			d.pconn.conn.SetReadBinary(false)
			d.writer.Close()

			// file list: unzip in final path
			if d.conf.isFilelist {
				if d.conf.SavePath != "" {
					srcf, err := os.Open(d.conf.SavePath + ".tmp")
					if err != nil {
						return err
					}

					destf, err := os.Create(d.conf.SavePath)
					if err != nil {
						srcf.Close()
						return err
					}

					_, err = io.Copy(destf, bzip2.NewReader(srcf))
					srcf.Close()
					destf.Close()
					if err != nil {
						return err
					}

					if err := os.Remove(d.conf.SavePath + ".tmp"); err != nil {
						return err
					}

				} else {
					cnt, err := ioutil.ReadAll(bzip2.NewReader(bytes.NewReader(d.content)))
					if err != nil {
						return err
					}
					d.content = cnt
				}

				// normal file
			} else {
				// validate
				if d.conf.SkipValidation == false && d.conf.Start == 0 && d.conf.Length <= 0 {
					dolog(LevelInfo, "[download] [%s] validating", d.conf.Peer.Nick)

					// file in disk
					var contentTTH TigerHash
					if d.conf.SavePath != "" {
						var err error
						contentTTH, err = TTHFromFile(d.conf.SavePath + ".tmp")
						if err != nil {
							return err
						}

						// file in ram
					} else {
						contentTTH = TTHFromBytes(d.content)
					}

					if contentTTH != d.conf.TTH {
						return fmt.Errorf("validation failed")
					}
				}

				// move to final path
				if d.conf.SavePath != "" {
					if err := os.Rename(d.conf.SavePath+".tmp", d.conf.SavePath); err != nil {
						return err
					}
				}
			}

			return errorTerminated
		}

	default:
		return fmt.Errorf("unhandled: %T %+v", msgi, msgi)
	}
	return nil
}

func (d *Download) handleExit(err error) {
	if d.terminateRequested != true && err != nil {
		dolog(LevelInfo, "ERR (download) [%s]: %s", d.conf.Peer.Nick, err)
	}

	delete(d.client.transfers, d)

	// free activedl and unlock next download
	delete(d.client.activeDownloadsByPeer, d.conf.Peer.Nick)
	for rot := range d.client.transfers {
		if od, ok := rot.(*Download); ok {
			if od.terminateRequested == false && od.state == "waiting_activedl" && d.conf.Peer == od.conf.Peer {
				od.state = "waited_activedl"
				od.client.activeDownloadsByPeer[od.conf.Peer.Nick] = d
				od.activeDlChan <- struct{}{}
				break
			}
		}
	}

	// free slot and unlock next download
	d.client.downloadSlotAvail += 1
	for rot := range d.client.transfers {
		if od, ok := rot.(*Download); ok {
			if od.terminateRequested == false && od.state == "waiting_slot" {
				od.state = "waited_slot"
				od.client.downloadSlotAvail -= 1
				od.slotChan <- struct{}{}
				break
			}
		}
	}

	// call callbacks
	if err == nil {
		dolog(LevelInfo, "[download] [%s] finished %s (s=%d l=%d)",
			d.conf.Peer.Nick, dcReadableQuery(d.query), d.conf.Start, len(d.content))
		if d.client.OnDownloadSuccessful != nil {
			d.client.OnDownloadSuccessful(d)
		}
	} else {
		dolog(LevelInfo, "[download] [%s] failed %s", d.conf.Peer.Nick, dcReadableQuery(d.query))
		if d.client.OnDownloadError != nil {
			d.client.OnDownloadError(d)
		}
	}
}
