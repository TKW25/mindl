package main

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
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"

	. "github.com/MinoMino/mindl/plugins"

	"github.com/MinoMino/minprogress"
	log "github.com/Sirupsen/logrus"
)

var permission = 0755

var (
	ErrNilGenerator = errors.New("DownloadGenerator() returned nil on first call.")
)

type IODataHandler func(data []byte) error
type IOCloseHandler func() error

// Implements io.Writer and provides control of the flow of the data going
// through it. Can also register handlers that are called when we get data.
type IOController struct {
	io.Writer
	dataCallbacks  []IODataHandler
	closeCallbacks []IOCloseHandler
}

func (ioctrl *IOController) Write(p []byte) (int, error) {
	for _, cb := range ioctrl.dataCallbacks {
		if err := cb(p); err != nil {
			return 0, err
		}
	}
	if ioctrl.Writer != nil {
		return ioctrl.Writer.Write(p)
	} else {
		// Allow use as no-op writer.
		return len(p), nil
	}
}

func (ioctrl *IOController) Close() error {
	for _, cb := range ioctrl.closeCallbacks {
		if err := cb(); err != nil {
			return err
		}
	}

	if closer, ok := ioctrl.Writer.(io.WriteCloser); ok {
		return closer.Close()
	}

	return nil
}

func (ioctrl *IOController) RegisterDataCallback(cb IODataHandler) {
	ioctrl.dataCallbacks = append(ioctrl.dataCallbacks, cb)
}

func (ioctrl *IOController) RegisterCloseCallback(cb IOCloseHandler) {
	ioctrl.closeCallbacks = append(ioctrl.closeCallbacks, cb)
}

// plugins.Reporter implementation.
type DownloadReporter struct {
	plugin         Plugin
	saved          chan<- string
	reportCallback IODataHandler
	// Other callbacks.
	callbacks []IODataHandler
	dirm      sync.Mutex
}

func (dr *DownloadReporter) FileWriter(dst string, report bool) (w io.WriteCloser, err error) {
	if filepath.IsAbs(dst) {
		return nil, fmt.Errorf("Not a relative path: %s", dst)
	}
	// Create the directories if we have to first.
	dst = filepath.Join(directory, dst)
	if err := dr.makeDirectories(dst); err != nil {
		return nil, err
	}

	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}

	ioctrl := &IOController{Writer: f}
	for _, cb := range dr.callbacks {
		ioctrl.RegisterDataCallback(cb)
	}
	// Report when we close the file.
	ioctrl.RegisterCloseCallback(func() error {
		dr.saved <- dst
		return nil
	})

	if report {
		ioctrl.RegisterDataCallback(dr.reportCallback)
	}

	return ioctrl, nil
}

func (dr *DownloadReporter) Copy(dst io.Writer, src io.Reader) (written int64, err error) {
	return dr.copy(dst, src, true)
}

func (dr *DownloadReporter) copy(dst io.Writer, src io.Reader, report bool) (written int64, err error) {
	ioctrl := &IOController{Writer: dst}
	dst = ioctrl
	for _, cb := range dr.callbacks {
		ioctrl.RegisterDataCallback(cb)
	}
	if report {
		ioctrl.RegisterDataCallback(dr.reportCallback)
	}

	buf := make([]byte, 4*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}

	return written, err
}

func (dr *DownloadReporter) SaveData(dst string, src io.Reader, report bool) (int64, error) {
	if filepath.IsAbs(dst) {
		return 0, fmt.Errorf("Not a relative path: %s", dst)
	}
	// Create the directories if we have to first.
	dst = filepath.Join(directory, dst)
	if err := dr.makeDirectories(dst); err != nil {
		return 0, err
	}

	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if n, err := dr.copy(f, src, report); err != nil {
		return n, err
	} else {
		// Tell the manager we got a file.
		dr.saved <- dst
		return n, err
	}
}

func (dr *DownloadReporter) SaveFile(dst, src string) (int64, error) {
	if filepath.IsAbs(dst) {
		return 0, fmt.Errorf("Not a relative path: %s", dst)
	}

	// Make sure src exists and get its size.
	info, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	// Create the directories if we have to first.
	dst = filepath.Join(directory, dst)
	if err = dr.makeDirectories(dst); err != nil {
		return 0, err
	} else if err = os.Rename(src, dst); err != nil {
		return 0, err
	}

	dr.saved <- dst
	return info.Size(), nil
}

func (dr *DownloadReporter) TempFile() (f *os.File, err error) {
	f, err = ioutil.TempFile(filepath.Join(directory, ".tmp"), fmt.Sprintf("mindl-%s-", dr.plugin.Name()))
	if err != nil {
		log.WithField("path", f.Name()).Debugf("Temporary file created.")
	}
	return
}

func (dr *DownloadReporter) makeDirectories(path string) error {
	dir := filepath.Dir(path)
	dr.dirm.Lock()
	defer dr.dirm.Unlock()
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			log.WithField("path", dir).Debug("Creating non-existing directories.")
			if err = os.MkdirAll(dir, os.FileMode(permission)); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	return nil
}

func assertRelative(path string) error {
	if filepath.IsAbs(path) {
		return fmt.Errorf("Not a relative path: %s", path)
	}

	return nil
}

// The manager itself.

type DownloadManager struct {
	progress *minprogress.ProgressBar
	paths    []string
	plugin   Plugin
	m        sync.Mutex
}

func NewDownloadManager(plugin Plugin) *DownloadManager {
	return &DownloadManager{
		plugin: plugin,
	}
}

func (dm *DownloadManager) Download(url string, maxWorkers int) ([]string, error) {
	var dlCount int
	dlgen, total := dm.plugin.DownloadGenerator(url)
	if dlgen == nil {
		panic(ErrNilGenerator)
	}

	if total == UnknownTotal {
		dm.progress = minprogress.NewProgressBar(minprogress.UnknownTotal)
	} else {
		dm.progress = minprogress.NewProgressBar(total)
	}
	dm.progress.SpeedUnits = minprogress.DataUnits
	dm.progress.Unit = "file"
	dm.progress.Units = "files"
	dm.progress.ReportsPerSample = 8 * maxWorkers
	next := dlgen()
	// nil or error to signal the goroutines are done.
	done := make(chan error)
	// Report the paths to the files as they're done and written to disk.
	got := make(chan string, maxWorkers)
	// Use a WaitGroup to make sure all goroutines finish before we exit on error.
	var wg sync.WaitGroup

	// Run a goroutine that spawns workers as needed.
	go func() {
		// Deal with potential panic by spawner.
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("Spawner panicked: %s", r)
				return
			}
		}()

		workerLimiter := make(chan struct{}, maxWorkers)
		ec := make(chan error, maxWorkers)
		for dlCount = 0; next != nil; dlCount++ {
			// Blocks until we have worker slots or we get an error.
			select {
			case err := <-ec:
				// Pass the error down the chain and return immediately.
				done <- err
				return
			case workerLimiter <- struct{}{}:
			}

			log.Debugf("Spawning worker #%d...", dlCount)
			// Spawn the worker and make sure we free a slot when done.
			wg.Add(1)
			go func(n int, dl Downloader) {
				// Deal with potential panic by the worker.
				defer func() {
					if r := recover(); r != nil {
						ec <- fmt.Errorf("Worker #%d panicked: %s", n, r)
					}
					wg.Done()
					return
				}()

				// Prepare the reporter for this particular worker.
				reporter := &DownloadReporter{
					plugin: dm.plugin,
					saved:  got,
					//callbacks: []IODataHandler{},
					reportCallback: func(data []byte) error {
						dm.progress.Report(n, len(data))
						return nil
					},
				}
				// Make sure we report we're done with the download regardless of what happens.
				defer dm.progress.Done(n)
				// Run the task.
				if err := dl(n, reporter); err != nil {
					ec <- err
					return
				}
				// Free the slot.
				<-workerLimiter
			}(dlCount, next)
			next = dlgen()
		}

		wg.Wait()

		// All workers are done, but we could still have errors buffered.
		select {
		case err := <-ec:
			done <- err
			return
		default:
		}

		if dlCount == 0 {
			done <- errors.New("Got no downloaders from the plugin.")
		} else {
			done <- nil
		}
	}()

	dm.m.Lock()
	// All the paths to the files that have been written to disk.
	dm.paths = make([]string, 0, 100)
	dm.m.Unlock()
loop:
	for {
		select {
		case err := <-done:
			if err != nil {
				return nil, err
			} else {
				break loop
			}
		case path := <-got:
			dm.m.Lock()
			dm.paths = append(dm.paths, path)
			dm.m.Unlock()
			// Report progress.
			dm.progress.Progress(1)
			log.Debug("Got file: " + path)
		}
	}

	return dm.paths, nil
}

func (dm *DownloadManager) ProgressString() string {
	var res string
	if dm.progress != nil {
		dm.m.Lock()
		dls := len(dm.paths)
		if dls != 0 {
			res = dm.progress.String() + " | Last: " + filepath.Base(dm.paths[len(dm.paths)-1])
		} else {
			res = dm.progress.String()
		}
		dm.m.Unlock()
	}

	return res
}