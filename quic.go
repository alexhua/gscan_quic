package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	quic "github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
)

var errNoSuchBucket = []byte("<?xml version='1.0' encoding='UTF-8'?><Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message></Error>")

func testQuic(ip string, config *ScanConfig, record *ScanRecord) bool {
	start := time.Now()

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return false
	}
	udpConn.SetDeadline(time.Now().Add(config.ScanMaxRTT))
	defer udpConn.Close()

	quicCfg := &quic.Config{
		HandshakeTimeout: config.HandshakeTimeout,
		KeepAlive:        false,
	}

	var serverName string
	if len(config.ServerName) == 0 {
		serverName = randomHost()
	} else {
		serverName = config.ServerName[rand.Intn(len(config.ServerName))]
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         serverName,
		NextProtos:         []string{"h3-29", "h3-27"},
	}

	udpAddr := &net.UDPAddr{IP: net.ParseIP(ip), Port: 443}
	host := net.JoinHostPort(serverName, "443")
	quicSessn, err := quic.DialEarly(udpConn, udpAddr, host, tlsCfg, quicCfg)
	if err != nil {
		return false
	}
	defer quicSessn.CloseWithError(quic.ErrorCode(256), "")

	// lv1 只会验证证书是否存在
	// quicSessn.HandshakeComplete()
	cs := quicSessn.ConnectionState()
	if !cs.HandshakeComplete {
		return false
	}
	pcs := cs.PeerCertificates
	if len(pcs) < 2 {
		return false
	}

	// lv2 验证证书是否正确
	if config.Level > 1 {
		pkp := pcs[1].SubjectKeyId
		if !bytes.Equal(g2pkp, pkp) && !bytes.Equal(g3pkp, pkp) { // && !bytes.Equal(g3ecc, pkp[:]) {
			return false
		}
	}

	// lv3 使用 http 访问来验证
	if config.Level > 2 {
		tr := &http3.RoundTripper{DisableCompression: false, TLSClientConfig: tlsCfg, QuicConfig: quicCfg}
		defer tr.Close()

		tr.Dial = func(network, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlySession, error) {
			return quicSessn, err
		}
		// 设置超时
		hclient := &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: config.ScanMaxRTT - time.Since(start),
		}
		url := "https://" + config.HTTPVerifyHosts[rand.Intn(len(config.HTTPVerifyHosts))]
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Close = true
		resp, _ := hclient.Do(req)
		if resp == nil || (resp.StatusCode < 200 || resp.StatusCode >= 400) || !strings.Contains(resp.Header.Get("Alt-Svc"), `h3-29=":443"`) {
			return false
		}
		if resp.Body != nil {
			defer resp.Body.Close()
			// lv4 验证是否是 NoSuchBucket 错误
			if config.Level > 3 && resp.Header.Get("Content-Type") == "application/xml; charset=UTF-8" { // 也许条件改为 || 更好
				body, err := ioutil.ReadAll(resp.Body)
				if err != nil || bytes.Equal(body, errNoSuchBucket) {
					return false
				}
			} else {
				io.Copy(ioutil.Discard, resp.Body)
			}
		}
	}

	if rtt := time.Since(start); rtt > config.ScanMinRTT {
		record.RTT += rtt
		return true
	}
	return false
}
