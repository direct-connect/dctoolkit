package main

import (
    "os"
    "strings"
    "io/ioutil"
    dctk "github.com/gswly/dctoolkit"
)

var ok = false

func client1() {
    client,err := dctk.NewClient(dctk.ClientConf{
        HubUrl: os.Getenv("HUBURL"),
        Nick: "client1",
        PrivateIp: true,
        TcpPort: 3006,
        UdpPort: 3006,
        PeerEncryptionMode: dctk.DisableEncryption,
        HubManualConnect: true,
    })
    if err != nil {
        panic(err)
    }

    os.Mkdir("/share", 0755)
    ioutil.WriteFile("/share/test file.txt", []byte(strings.Repeat("A", 10000)), 0644)

    client.OnInitialized = func() {
        client.ShareAdd("share", "/share")
    }

    client.OnShareIndexed = func() {
        client.HubConnect()
    }

    client.Run()
}

func client2() {
    client,err := dctk.NewClient(dctk.ClientConf{
        HubUrl: os.Getenv("HUBURL"),
        Nick: "client2",
        PrivateIp: true,
        TcpPort: 3005,
        UdpPort: 3005,
        PeerEncryptionMode: dctk.DisableEncryption,
    })
    if err != nil {
        panic(err)
    }

    client.OnPeerConnected = func(p *dctk.Peer) {
        if p.Nick == "client1" {
            client.DownloadFile(dctk.DownloadConf{
                Peer: p,
                TTH: "UJUIOGYVALWRB56PRJEB6ZH3G4OLTELOEQ3UKMY",
            })
        }
    }

    client.OnDownloadSuccessful = func(d* dctk.Download) {
        ok = true
        client.Terminate()
    }

    client.Run()
}

func main() {
    dctk.SetLogLevel(dctk.LevelDebug)

    go client1()
    client2()

    if ok == false {
        panic("test failed")
    }
}