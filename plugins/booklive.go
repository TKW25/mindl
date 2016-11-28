package plugins

// mindl - A downloader for various sites and services.
// Copyright (C) 2016  Mino <mino@minomino.org>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/MinoMino/mindl/plugins/binb"
	log "github.com/Sirupsen/logrus"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrBookLiveUnknownCid  = errors.New("CID format not <title_id>_<volume>.")
	ErrBookLiveUnknownUrl  = errors.New("URL could not be parsed.")
	ErrBookLiveFailedLogin = errors.New("Failed to login. Wrong credentials?")
	ErrBookLiveLoginScreen = errors.New("Error while getting login token.")
)

var booklive = BookLive{
	[]Option{
		&StringOption{K: "Username", Required: true},
		&StringOption{K: "Password", Required: true},
		&BoolOption{K: "Lossless", V: false,
			C: "If set to true, save as PNG. Original images are in JPEG, so you can't escape some artifacts even with this on."},
		&IntOption{K: "JPEGQuality", V: 95,
			C: "Does nothing if Lossless is on. >95 not adviced, as it increases file size a ton with little improvement."},
		&BoolOption{K: "Metadata", V: true},
	},
}

const (
	urlApi         = "https://booklive.jp/bib-api/"
	urlLoginScreen = "https://booklive.jp/login"
	urlLogin       = "https://booklive.jp/login/index"
)

var urlBookLive, _ = url.ParseRequestURI("https://booklive.jp/")

var reBook = regexp.MustCompile(`^https?://booklive.jp/product/index/title_id/(?P<title_id>[0-9]+?)/vol_no/(?P<volume>[0-9]+?)$`)
var reReader = regexp.MustCompile(`^https?://booklive.jp/bviewer/\?cid=(?P<cid>[_0-9]+)`)
var reTokenSearch = regexp.MustCompile(`input type="hidden" name="token" value="(.+?)">`)

type BookLive struct {
	options []Option
}

func (bl *BookLive) Name() string {
	return "BookLive"
}

func (bl *BookLive) Version() string {
	return ""
}

func (bl *BookLive) CanHandle(url string) bool {
	return (reBook.MatchString(url) || reReader.MatchString(url))
}

func (bl *BookLive) Options() []Option {
	return bl.options
}

func (bl *BookLive) DownloadGenerator(url string) (dlgen func() Downloader, length int) {
	// Initialization.
	cid, volume := bl.getCidAndVolume(url)
	opts := OptionsToMap(bl.options)
	client := NewHTTPClient(20)
	bl.login(client, opts["Username"].(string), opts["Password"].(string))
	api := binb.NewApi(urlApi, cid, client, nil)
	if err := api.GetContent(); err != nil {
		panic(err)
	}
	length = len(api.Pages)
	dir := norm.NFD.String(fmt.Sprintf("%s 第%02d巻", api.ContentInfo.Title, volume))

	i := 0
	// Generator.
	dlgen = func() Downloader {
		if i >= length {
			return nil
		}

		i++
		// Downloader
		return func(n int, rep Reporter) error {
			r, err := api.GetImage(n)
			if err != nil {
				return err
			}
			defer r.Close()

			buf := &bytes.Buffer{}
			// Download through the reporter.
			if _, err := rep.Copy(buf, r); err != nil {
				return err
			}

			img, err := api.Descrambler.Descramble(api.Pages[n], buf)
			buf.Reset()
			w, err := rep.FileWriter(filepath.Join(dir, fmt.Sprintf("%04d.png", n+1)), false)
			if err != nil {
				panic(err)
			}
			defer w.Close()
			png.Encode(w, img)

			return nil
		}
	}
	return
}

func (bl *BookLive) login(client *http.Client, username, password string) {
	// First we get a login token.
	var token string

	r, err := client.Do(NewGetRequest(urlLoginScreen))
	if err != nil {
		log.Error(err)
		panic(ErrBookLiveLoginScreen)
	}
	PanicForStatus(r, "")

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Error(err)
		panic(ErrBookLiveLoginScreen)
	}
	if re := reTokenSearch.FindStringSubmatch(string(body)); re == nil {
		log.Error("Found no login token.")
		panic(ErrBookLiveLoginScreen)
	} else {
		token = re[1]
	}

	// Then we login.
	log.WithFields(log.Fields{"token": token,
		"username": username}).Debug("Logging in...")
	r, err = client.Do(NewPostFormRequest(urlLogin, url.Values{
		"mail_addr": {username},
		"pswd":      {password},
		"token":     {token},
	}))
	if err != nil {
		log.Error(err)
		panic(ErrBookLiveFailedLogin)
	}
	// http.Client does not follow 301s on POST, but server does reply with it.
	if r.StatusCode != http.StatusMovedPermanently {
		PanicForStatus(r, "Incorrect credentials?")
	}

	// Confirm we logged in by checking cookies.
	var logged bool
	for _, cookie := range client.Jar.Cookies(urlBookLive) {
		if cookie.Name == "BL_LI" {
			log.WithField("session", cookie.Value).Debug("Logged in!")
			logged = true
			break
		}
	}
	if !logged {
		panic(ErrBookLiveFailedLogin)
	}
}

func (bl *BookLive) getCidAndVolume(url string) (cid string, volume int) {
	var err error
	if re := reBook.FindStringSubmatch(url); re != nil {
		cid = re[1] + "_" + re[2]
		volume, err = strconv.Atoi(re[2])
		if err != nil {
			panic(err)
		}
	} else if re := reReader.FindStringSubmatch(url); re != nil {
		cid = re[1]
		split := strings.Split(cid, "_")
		if len(split) < 2 {
			panic(ErrBookLiveUnknownCid)
		}

		volume, err = strconv.Atoi(split[1])
		if err != nil {
			panic(err)
		}
	} else {
		// Should never happen.
		panic(ErrBookLiveUnknownUrl)
	}

	return
}