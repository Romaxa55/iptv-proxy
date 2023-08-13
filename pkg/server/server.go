/*
 * Iptv-Proxy is a project to proxyfie an m3u file and to proxyfie an Xtream iptv service (client API).
 * Copyright (C) 2020  Pierre-Emmanuel Jacquier
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"bytes"
	"fmt"
	"github.com/gin-contrib/cors"
	"github.com/grafov/m3u8"
	"github.com/jamesnetherton/m3u"
	"github.com/romaxa55/iptv-proxy/pkg/config"
	uuid "github.com/satori/go.uuid"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

var defaultProxyfiedM3UPath = filepath.Join(os.TempDir(), uuid.NewV4().String()+".iptv-proxy.m3u")
var endpointAntiColision = "a6d7e846"

type SegmentMapping struct {
	OriginalURI   string
	DownloadedURI string
}

const downloadDir = "hlsdownloads"

// Config represent the server configuration
type Config struct {
	*config.ProxyConfig

	// M3U service part
	playlist *m3u.Playlist
	// this variable is set only for m3u proxy endpoints
	track *m3u.Track
	// path to the proxyfied m3u file
	proxyfiedM3UPath string

	endpointAntiColision string
}

// NewServer initialize a new server configuration
func NewServer(config *config.ProxyConfig) (*Config, error) {
	var p m3u.Playlist
	if config.RemoteURL.String() != "" {
		var err error
		p, err = m3u.Parse(config.RemoteURL.String())
		if err != nil {
			return nil, err
		}
	}

	if trimmedCustomId := strings.Trim(config.CustomId, "/"); trimmedCustomId != "" {
		endpointAntiColision = trimmedCustomId
	}

	return &Config{
		config,
		&p,
		nil,
		defaultProxyfiedM3UPath,
		endpointAntiColision,
	}, nil
}

// Serve the iptv-proxy api
func (c *Config) Serve() error {
	if err := c.playlistInitialization(); err != nil {
		return err
	}

	router := gin.Default()
	router.Use(cors.Default())
	group := router.Group("/")
	c.routes(group)

	return router.Run(fmt.Sprintf(":%d", c.HostConfig.Port))
}

func (c *Config) playlistInitialization() error {
	if len(c.playlist.Tracks) == 0 {
		return nil
	}

	f, err := os.Create(c.proxyfiedM3UPath)
	if err != nil {
		return err
	}
	defer func(f *os.File) {
		_ = f.Close()
	}(f)

	return c.marshallInto(f, false)
}

// MarshallInto a *bufio.Writer a Playlist.
func (c *Config) marshallInto(into *os.File, xtream bool) error {
	filteredTrack := make([]m3u.Track, 0, len(c.playlist.Tracks))

	ret := 0
	_, _ = into.WriteString("#EXTM3U\n") // nolint: errcheck
	for i, track := range c.playlist.Tracks {
		var buffer bytes.Buffer

		buffer.WriteString("#EXTINF:")                       // nolint: errcheck
		buffer.WriteString(fmt.Sprintf("%d ", track.Length)) // nolint: errcheck
		for i := range track.Tags {
			if i == len(track.Tags)-1 {
				buffer.WriteString(fmt.Sprintf("%s=%q", track.Tags[i].Name, track.Tags[i].Value)) // nolint: errcheck
				continue
			}
			buffer.WriteString(fmt.Sprintf("%s=%q ", track.Tags[i].Name, track.Tags[i].Value)) // nolint: errcheck
		}

		uri, err := c.replaceURL(track.URI, i-ret, xtream)
		if err != nil {
			ret++
			log.Printf("ERROR: track: %s: %s", track.Name, err)
			continue
		}

		_, _ = into.WriteString(fmt.Sprintf("%s, %s\n%s\n", buffer.String(), track.Name, uri)) // nolint: errcheck

		filteredTrack = append(filteredTrack, track)
	}
	c.playlist.Tracks = filteredTrack

	return into.Sync()
}

// ReplaceURL replace original playlist url by proxy url
func (c *Config) replaceURL(uri string, trackIndex int, xtream bool) (string, error) {
	oriURL, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	protocol := "http"
	if c.HTTPS {
		protocol = "https"
	}

	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = fmt.Sprintf("/%s", customEnd)
	}

	uriPath := oriURL.EscapedPath()
	if xtream {
		uriPath = strings.ReplaceAll(uriPath, c.XtreamUser.PathEscape(), c.User.PathEscape())
		uriPath = strings.ReplaceAll(uriPath, c.XtreamPassword.PathEscape(), c.Password.PathEscape())
	} else {
		uriPath = path.Join("/", c.endpointAntiColision, c.User.PathEscape(), c.Password.PathEscape(), fmt.Sprintf("%d", trackIndex), path.Base(uriPath))
	}

	basicAuth := oriURL.User.String()
	if basicAuth != "" {
		basicAuth += "@"
	}

	newURI := fmt.Sprintf(
		"%s://%s%s:%d%s%s",
		protocol,
		basicAuth,
		c.HostConfig.Hostname,
		c.AdvertisedPort,
		customEnd,
		uriPath,
	)

	newURL, err := url.Parse(newURI)
	if err != nil {
		return "", err
	}

	return newURL.String(), nil
}

func downloadSegments(mappings []*SegmentMapping) {
	var wg sync.WaitGroup
	ch := make(chan *SegmentMapping, len(mappings))

	for _, mapping := range mappings {
		wg.Add(1)
		go downloadSegment(mapping, &wg, ch)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	for downloadedMapping := range ch {
		for _, mapping := range mappings {
			if mapping.OriginalURI == downloadedMapping.OriginalURI {
				mapping.DownloadedURI = downloadedMapping.DownloadedURI
				break
			}
		}
	}
}

func downloadSegment(mapping *SegmentMapping, wg *sync.WaitGroup, ch chan<- *SegmentMapping) {
	defer wg.Done()

	resp, err := http.Get(mapping.OriginalURI)
	if err != nil {
		log.Printf("Ошибка при скачивании %s: %v", mapping.OriginalURI, err)
		return
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	// Создаем директорию, если она не существует
	if _, err := os.Stat("hlsdownloads"); os.IsNotExist(err) {
		_ = os.Mkdir("hlsdownloads", 0755)
	}

	filename := filepath.Join(downloadDir, cleanFilename(mapping.OriginalURI))
	file, err := os.Create(filename)
	if err != nil {
		log.Printf("Ошибка при создании файла %s: %v", filename, err)
		return
	}

	defer func(file *os.File) {
		_ = file.Close()
	}(file)

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		log.Printf("Ошибка при записи в файл %s: %v", filename, err)
		return
	}

	mapping.DownloadedURI = "/" + filename
	ch <- mapping
}

func downloadSegmentsFromPlaylist(p *m3u8.MediaPlaylist, listType m3u8.ListType) *m3u8.MediaPlaylist {
	if listType != m3u8.MEDIA {
		log.Println("Поддерживается только тип списка MEDIA")
		return p
	}

	// Создаем список структур SegmentMapping для каждого сегмента
	var mappings []*SegmentMapping
	for _, seg := range p.Segments {
		if seg != nil {
			mappings = append(mappings, &SegmentMapping{OriginalURI: seg.URI})
		}
	}

	// Загружаем сегменты
	downloadSegments(mappings)

	for _, seg := range p.Segments {
		for _, mapping := range mappings {
			if seg != nil && seg.URI == mapping.OriginalURI {
				seg.URI = mapping.DownloadedURI
				break
			}
		}
	}

	return p
}

func cleanFilename(url string) string {
	base := filepath.Base(url)         // извлекаем базовое имя файла из URL
	return strings.Split(base, "?")[0] // убираем все после знака "?"
}
