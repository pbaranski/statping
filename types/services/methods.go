package services

import (
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"github.com/statping/statping/types"
	"github.com/statping/statping/types/errors"
	"github.com/statping/statping/types/failures"
	"github.com/statping/statping/types/hits"
	"github.com/statping/statping/utils"
	"io/ioutil"
	"sort"
	"strconv"
	"time"
)

const limitedFailures = 25

func (s *Service) LoadTLSCert() (*tls.Config, error) {
	if !s.TLSCert.Valid && !s.TLSCertKey.Valid {
		return nil, nil
	}

	// load TLS cert and key from file path or PEM format
	var cert tls.Certificate
	var err error
	tlsCertExtension := utils.FileExtension(s.TLSCert.String)
	tlsCertKeyExtension := utils.FileExtension(s.TLSCertKey.String)
	if tlsCertExtension == "" && tlsCertKeyExtension == "" {
		cert, err = tls.X509KeyPair([]byte(s.TLSCert.String), []byte(s.TLSCertKey.String))
	} else {
		cert, err = tls.LoadX509KeyPair(s.TLSCert.String, s.TLSCertKey.String)
	}
	if err != nil {
		return nil, errors.Wrap(err, "issue loading X509KeyPair")
	}

	// create Root CA pool or use Root CA provided
	chainFile := s.TLSCert.String
	if s.TLSCertRoot.String != "" {
		chainFile = s.TLSCertRoot.String
	}
	caCert, err := ioutil.ReadFile(chainFile)
	if err != nil {
		return nil, errors.Wrap(err, "issue reading cert file: "+chainFile)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func (s *Service) Duration() time.Duration {
	return time.Duration(s.Interval) * time.Second
}

// Start will create a channel for the service checking go routine
func (s *Service) UptimeData(hits []*hits.Hit, fails []*failures.Failure) (*UptimeSeries, error) {
	if len(hits) == 0 {
		return nil, errors.New("service does not have any successful hits")
	}
	// if theres no failures, then its been online 100%,
	// return a series from created time, to current.
	if len(fails) == 0 {
		fistHit := hits[0]
		duration := utils.Now().Sub(fistHit.CreatedAt).Milliseconds()
		set := []series{
			{
				Start:    fistHit.CreatedAt,
				End:      utils.Now(),
				Duration: duration,
				Online:   true,
			},
		}
		out := &UptimeSeries{
			Start:    fistHit.CreatedAt,
			End:      utils.Now(),
			Uptime:   duration,
			Downtime: 0,
			Series:   set,
		}
		return out, nil
	}

	tMap := make(map[time.Time]bool)

	for _, v := range hits {
		tMap[v.CreatedAt] = true
	}
	for _, v := range fails {
		tMap[v.CreatedAt] = false
	}

	var servs []ser
	for t, v := range tMap {
		s := ser{
			Time:   t,
			Online: v,
		}
		servs = append(servs, s)
	}
	if len(servs) == 0 {
		return nil, errors.New("error generating uptime data structure")
	}
	sort.Sort(ByTime(servs))

	var allTimes []series
	online := servs[0].Online
	thisTime := servs[0].Time
	for i := 0; i < len(servs); i++ {
		v := servs[i]
		if v.Online != online {
			s := series{
				Start:    thisTime,
				End:      v.Time,
				Duration: v.Time.Sub(thisTime).Milliseconds(),
				Online:   online,
			}
			allTimes = append(allTimes, s)
			thisTime = v.Time
			online = v.Online
		}
	}
	if len(allTimes) == 0 {
		return nil, errors.New("error generating uptime series structure")
	}

	first := servs[0].Time
	last := servs[len(servs)-1].Time
	if !s.Online {
		s := series{
			Start:    allTimes[len(allTimes)-1].End,
			End:      utils.Now(),
			Duration: utils.Now().Sub(last).Milliseconds(),
			Online:   s.Online,
		}
		allTimes = append(allTimes, s)
	} else {
		l := allTimes[len(allTimes)-1]
		s := series{
			Start:    l.Start,
			End:      utils.Now(),
			Duration: utils.Now().Sub(l.Start).Milliseconds(),
			Online:   true,
		}
		allTimes = append(allTimes, s)
	}

	response := &UptimeSeries{
		Start:    first,
		End:      last,
		Uptime:   addDurations(allTimes, true),
		Downtime: addDurations(allTimes, false),
		Series:   allTimes,
	}

	return response, nil
}

func addDurations(s []series, on bool) int64 {
	var dur int64
	for _, v := range s {
		if v.Online == on {
			dur += v.Duration
		}
	}
	return dur
}

type ser struct {
	Time   time.Time
	Online bool
}

type UptimeSeries struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Uptime   int64     `json:"uptime"`
	Downtime int64     `json:"downtime"`
	Series   []series  `json:"series"`
}

type ByTime []ser

func (a ByTime) Len() int           { return len(a) }
func (a ByTime) Less(i, j int) bool { return a[i].Time.Before(a[j].Time) }
func (a ByTime) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type series struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Duration int64     `json:"duration"`
	Online   bool      `json:"online"`
}

// Start will create a channel for the service checking go routine
func (s *Service) Start() {
	if s.IsRunning() {
		return
	}
	s.Running = make(chan bool)
}

// Close will stop the go routine that is checking if service is online or not
func (s *Service) Close() {
	if s.IsRunning() {
		close(s.Running)
	}
}

func humanMicro(val int64) string {
	if val < 10000 {
		return fmt.Sprintf("%d μs", val)
	}
	return fmt.Sprintf("%0.0f ms", float64(val)*0.001)
}

// IsRunning returns true if the service go routine is running
func (s *Service) IsRunning() bool {
	if s.Running == nil {
		return false
	}
	select {
	case <-s.Running:
		return false
	default:
		return true
	}
}

func (s Service) Hash() string {
	format := fmt.Sprintf("name:%sdomain:%sport:%dtype:%smethod:%s", s.Name, s.Domain, s.Port, s.Type, s.Method)
	h := sha1.New()
	h.Write([]byte(format))
	return hex.EncodeToString(h.Sum(nil))
}

// SelectAllServices returns a slice of *core.Service to be store on []*core.Services
// should only be called once on startup.
func SelectAllServices(start bool) (map[int64]*Service, error) {
	if len(allServices) > 0 {
		return allServices, nil
	}

	for _, s := range all() {

		if start {
			CheckinProcess(s)
		}

		fails := s.AllFailures().LastAmount(limitedFailures)
		s.Failures = fails

		for _, c := range s.Checkins() {
			s.AllCheckins = append(s.AllCheckins, c)
		}

		// collect initial service stats
		s.UpdateStats()

		allServices[s.Id] = s
	}

	return allServices, nil
}

func (s *Service) UpdateStats() *Service {
	s.Online24Hours = s.OnlineDaysPercent(1)
	s.Online7Days = s.OnlineDaysPercent(7)
	s.AvgResponse = s.AvgTime()
	s.FailuresLast24Hours = s.FailuresSince(utils.Now().Add(-time.Hour * 24)).Count()

	if s.LastOffline.IsZero() {
		lastFail := s.AllFailures().Last()
		if lastFail != nil {
			s.LastOffline = lastFail.CreatedAt
		}
	}

	s.Stats = &Stats{
		Failures: s.AllFailures().Count(),
		Hits:     s.AllHits().Count(),
		FirstHit: s.FirstHit().CreatedAt,
	}
	return s
}

// AvgTime will return the average amount of time for a service to response back successfully
func (s *Service) AvgTime() int64 {
	return s.AllHits().Avg()
}

// OnlineDaysPercent returns the service's uptime percent within last 24 hours
func (s *Service) OnlineDaysPercent(days int) float32 {
	ago := utils.Now().Add(-time.Duration(days) * types.Day)
	return s.OnlineSince(ago)
}

// OnlineSince accepts a time since parameter to return the percent of a service's uptime.
func (s *Service) OnlineSince(ago time.Time) float32 {
	failed := s.FailuresSince(ago)
	failsList := failed.Count()

	total := s.HitsSince(ago)
	hitsList := total.Count()

	if failsList == 0 {
		s.Online24Hours = 100.00
		return s.Online24Hours
	}

	if hitsList == 0 {
		s.Online24Hours = 0
		return s.Online24Hours
	}

	avg := (float64(failsList) / float64(hitsList)) * 100
	avg = 100 - avg
	if avg < 0 {
		avg = 0
	}
	amount, _ := strconv.ParseFloat(fmt.Sprintf("%0.2f", avg), 10)
	s.Online24Hours = float32(amount)
	return s.Online24Hours
}

// Downtime returns the amount of time of a offline service
func (s *Service) Downtime() time.Duration {
	hit := s.LastHit()
	fail := s.AllFailures().Last()
	if hit == nil {
		return time.Duration(0)
	}
	if fail == nil {
		return utils.Now().Sub(hit.CreatedAt)
	}

	return fail.CreatedAt.Sub(hit.CreatedAt)
}
