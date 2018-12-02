package main

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	statsd "github.com/DataDog/datadog-go/statsd"
	log "github.com/Sirupsen/logrus"
)

const sampleRate = 1.0

const (
	routerMsg int = iota
	scalingMsg
	dynoSampleMsg
	pgSampleMsg
	redisSampleMsg
	releaseMsg
)

var routerMetricsKeys = []string{"dyno", "method", "status", "host", "code", "desc", "at"}
var sampleMetricsKeys = []string{"source", "dyno"}
var scalingMetricsKeys = []string{"mailer", "web"}

type Client struct {
	*statsd.Client
	ExcludedTags map[string]bool
}

var statusCode *regexp.Regexp = regexp.MustCompile(`^(?P<Family>\d)\d\d`)

func statsdClient(addr string) (*Client, error) {

	c, err := statsd.New(addr)
	return &Client{c, make(map[string]bool)}, err
}

func (c *Client) sendToStatsd(in chan *logMetrics) {

	var data *logMetrics
	var ok bool
	for {
		data, ok = <-in

		if !ok { //Exit, channel was closed
			return
		}

		log.WithFields(log.Fields{
			"type":   data.typ,
			"app":    data.app,
			"tags":   data.tags,
			"prefix": data.prefix,
		}).Debug("logMetrics received")

		if data.typ == routerMsg {
			c.sendRouterMsg(data)
		} else if data.typ == dynoSampleMsg {
			c.sendSampleMsg(data, "dyno")
		} else if data.typ == pgSampleMsg {
			c.sendSampleMsg(data, "postgres")
		} else if data.typ == redisSampleMsg {
			c.sendSampleMsg(data, "redis")
		} else if data.typ == scalingMsg {
			c.sendEvents(*data.app, "heroku", data.events, *data.tags)
			c.sendScalingMsg(data)
		} else if data.typ == releaseMsg {
			c.sendEvents(*data.app, "app", data.events, *data.tags)
		} else {
			log.WithField("type", data.typ).Warn("Unknown log message")
		}
	}
}

func (c *Client) sendEvents(app string, namespace string, events []string, tags []string) {
	for _, v := range events {
		event := statsd.NewEvent(namespace+"/api: "+app, v)
		event.Tags = tags
		c.Event(event)
		log.WithFields(log.Fields{
			"type":  "event",
			"app":   app,
			"value": v,
		}).Info("Event sent")
	}
}

func (c *Client) extractTags(tags []string, permittedTags []string, metrics map[string]logValue) []string {
	for _, mk := range permittedTags {
		if c.ExcludedTags[mk] {
			continue
		}
		if v, ok := metrics[mk]; ok {
			tags = append(tags, mk+":"+v.Val)
		}
	}
	sort.Strings(tags)
	return tags
}

func addStatusFamilyToTags(data *logMetrics, tags []string) []string {
	if val, ok := data.metrics["status"]; ok {
		match := statusCode.FindStringSubmatch(val.Val)
		if len(match) > 1 {
			tags = append(tags, "statusFamily:"+match[1]+"xx")
		}
	}
	return tags
}

func (c *Client) sendRouterMsg(data *logMetrics) {
	tags := c.extractTags(*data.tags, routerMetricsKeys, data.metrics)
	tags = addStatusFamilyToTags(data, tags)

	log.WithFields(log.Fields{
		"app":    *data.app,
		"tags":   *data.tags,
		"prefix": *data.prefix,
	}).Debug("sendRouterMsg")

	conn, err := strconv.ParseFloat(data.metrics["connect"].Val, 10)
	if err != nil {
		log.WithFields(log.Fields{
			"type":   "router",
			"err":    err,
			"metric": "connect",
		}).Info("Could not parse metric value")
		return
	}
	serv, err := strconv.ParseFloat(data.metrics["service"].Val, 10)
	if err != nil {
		log.WithFields(log.Fields{
			"type":   "router",
			"metric": "service",
			"err":    err,
		}).Info("Could not parse metric value")
		return
	}

	bytes, err := strconv.ParseFloat(data.metrics["bytes"].Val, 10)
	if err != nil {
		log.WithFields(log.Fields{
			"type":   "router",
			"metric": "bytes",
			"err":    err,
		}).Info("Could not parse metric value")
		return
	}
	// https://devcenter.heroku.com/articles/http-routing
	err = c.Histogram(*data.prefix+"heroku.router.response.bytes", bytes, tags, sampleRate)
	if err != nil {
		log.WithField("error", err).Info("Failed to send Histogram")
	}
	err = c.Histogram(*data.prefix+"heroku.router.request.connect", conn, tags, sampleRate)
	if err != nil {
		log.WithField("error", err).Info("Failed to send Histogram")
	}
	err = c.Histogram(*data.prefix+"heroku.router.request.service", serv, tags, sampleRate)
	if err != nil {
		log.WithField("error", err).Info("Failed to send Histogram")
	}
	if data.metrics["at"].Val == "error" {
		err = c.Count(*data.prefix+"heroku.router.error", 1, tags, 0.1)
		if err != nil {
			log.WithField("error", err).Info("Failed to send Count")
		}
	}
}

func (c *Client) sendSampleMsg(data *logMetrics, sampleType string) {
	tags := c.extractTags(*data.tags, sampleMetricsKeys, data.metrics)

	log.WithFields(log.Fields{
		"app":    *data.app,
		"tags":   tags,
		"prefix": *data.prefix,
	}).Debug("sendSampleMsg")

	for k, v := range data.metrics {

		if strings.Index(k, "#") != -1 {
			m := strings.Replace(strings.Split(k, "#")[1], "-", "_", -1)
			vnum, err := strconv.ParseFloat(v.Val, 10)
			if err == nil {
				err = c.Gauge(*data.prefix+"heroku."+sampleType+"."+m, vnum, tags, sampleRate)
				if err != nil {
					log.WithField("error", err).Info("Failed to send Gauge")
				}
			} else {
				log.WithFields(log.Fields{
					"type":   "sample",
					"metric": k,
					"err":    err,
				}).Info("Could not parse metric value")
			}
		}
	}
}

func (c *Client) sendScalingMsg(data *logMetrics) {
	tags := *data.tags

	log.WithFields(log.Fields{
		"app":    *data.app,
		"tags":   tags,
		"prefix": *data.prefix,
	}).Debug("sendScalingMsg")

	for _, mk := range scalingMetricsKeys {
		if v, ok := data.metrics[mk]; ok {
			vnum, err := strconv.ParseFloat(v.Val, 10)
			if err == nil {
				err = c.Gauge(*data.prefix+"heroku.dyno."+mk, vnum, tags, sampleRate)
				if err != nil {
					log.WithField("error", err).Info("Failed to send Gauge")
				}
			} else {
				log.WithFields(log.Fields{
					"type":   "scaling",
					"metric": mk,
					"err":    err,
				}).Info("Could not parse metric value")
			}
		}
	}
}

func (c *Client) sendMetric(metricType string, metricName string, value float64, tags []string) error {
	switch metricType {
	case "metric", "sample":
		return c.Gauge(metricName, value, tags, sampleRate)
	case "measure":
		return c.Histogram(metricName, value, tags, sampleRate)
	case "count":
		return c.Count(metricName, int64(value), tags, sampleRate)
	default:
		return errors.New("Unknown metric type" + metricType)
	}
}
