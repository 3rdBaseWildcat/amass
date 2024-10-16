// Copyright © by Jeff Foley 2017-2024. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/caffix/stringset"
	et "github.com/owasp-amass/amass/v4/engine/types"
	"github.com/owasp-amass/amass/v4/utils/net/http"
	dbt "github.com/owasp-amass/asset-db/types"
	"github.com/owasp-amass/engine/plugins/support"
	oam "github.com/owasp-amass/open-asset-model"
	"github.com/owasp-amass/open-asset-model/domain"
	"github.com/owasp-amass/open-asset-model/source"
	"go.uber.org/ratelimit"
)

type binaryEdge struct {
	name   string
	log    *slog.Logger
	rlimit ratelimit.Limiter
	source *source.Source
}

func NewBinaryEdge() et.Plugin {
	return &binaryEdge{
		name:   "BinaryEdge",
		rlimit: ratelimit.New(10, ratelimit.WithoutSlack),
		source: &source.Source{
			Name:       "BinaryEdge",
			Confidence: 80,
		},
	}
}

func (be *binaryEdge) Name() string {
	return be.name
}

func (be *binaryEdge) Start(r et.Registry) error {
	be.log = r.Log().WithGroup("plugin").With("name", be.name)

	if err := r.RegisterHandler(&et.Handler{
		Plugin:       be,
		Name:         be.name + "-Handler",
		Priority:     5,
		MaxInstances: 10,
		Transforms:   []string{string(oam.FQDN)},
		EventType:    oam.FQDN,
		Callback:     be.check,
	}); err != nil {
		return err
	}

	be.log.Info("Plugin started")
	return nil
}

func (be *binaryEdge) Stop() {
	be.log.Info("Plugin stopped")
}

func (be *binaryEdge) check(e *et.Event) error {
	fqdn, ok := e.Asset.Asset.(*domain.FQDN)
	if !ok {
		return errors.New("failed to extract the FQDN asset")
	}

	ds := e.Session.Config().GetDataSourceConfig(be.name)
	if ds == nil || len(ds.Creds) == 0 {
		return nil
	}

	var keys []string
	for _, cr := range ds.Creds {
		if cr != nil && cr.Apikey != "" {
			keys = append(keys, cr.Apikey)
		}
	}

	if a, conf := e.Session.Scope().IsAssetInScope(fqdn, 0); conf > 0 || a == nil {
		return nil
	} else if f, ok := a.(*domain.FQDN); !ok || f == nil || !strings.EqualFold(fqdn.Name, f.Name) {
		return nil
	}

	src := support.GetSource(e.Session, be.source)
	if src == nil {
		return errors.New("failed to obtain the plugin source information")
	}

	since, err := support.TTLStartTime(e.Session.Config(), string(oam.FQDN), string(oam.FQDN), be.name)
	if err != nil {
		return err
	}

	var names []*dbt.Asset
	if support.AssetMonitoredWithinTTL(e.Session, e.Asset, src, since) {
		names = append(names, be.lookup(e, fqdn.Name, src, since)...)
	} else {
		names = append(names, be.query(e, fqdn.Name, src, keys)...)
		support.MarkAssetMonitored(e.Session, e.Asset, src)
	}

	if len(names) > 0 {
		be.process(e, names, src)
	}
	return nil
}

func (be *binaryEdge) lookup(e *et.Event, name string, src *dbt.Asset, since time.Time) []*dbt.Asset {
	return support.SourceToAssetsWithinTTL(e.Session, name, string(oam.FQDN), src, since)
}

func (be *binaryEdge) query(e *et.Event, name string, src *dbt.Asset, keys []string) []*dbt.Asset {
	subs := stringset.New()
	defer subs.Close()

	pagenum := 1
loop:
	for _, key := range keys {
		for pagenum <= 500 {
			be.rlimit.Take()
			resp, err := http.RequestWebPage(context.TODO(), &http.Request{
				Header: http.Header{"X-KEY": []string{key}},
				URL:    "https://api.binaryedge.io/v2/query/domains/subdomain/" + name + "?page=" + strconv.Itoa(pagenum),
			})
			if err != nil || resp.Body == "" {
				break
			}

			var j struct {
				Results struct {
					Page     int      `json:"page"`
					PageSize int      `json:"pagesize"`
					Total    int      `json:"total"`
					Events   []string `json:"events"`
				} `json:"results"`
			}
			if err := json.Unmarshal([]byte("{\"results\":"+resp.Body+"}"), &j); err != nil {
				break
			}

			for _, n := range j.Results.Events {
				nstr := strings.ToLower(strings.TrimSpace(n))
				// if the subdomain is not in scope, skip it
				if _, conf := e.Session.Scope().IsAssetInScope(&domain.FQDN{Name: nstr}, 0); conf > 0 {
					subs.Insert(nstr)
				}
			}

			if j.Results.Page > 0 && j.Results.Page <= 500 && j.Results.PageSize > 0 &&
				j.Results.Total > 0 && j.Results.Page <= (j.Results.Total/j.Results.PageSize) {
				pagenum++
			} else {
				break loop
			}
		}
	}

	return be.store(e, subs.Slice(), src)
}

func (be *binaryEdge) store(e *et.Event, names []string, src *dbt.Asset) []*dbt.Asset {
	return support.StoreFQDNsWithSource(e.Session, names, src, be.name, be.name+"-Handler")
}

func (be *binaryEdge) process(e *et.Event, assets []*dbt.Asset, src *dbt.Asset) {
	support.ProcessFQDNsWithSource(e, assets, src)
}
