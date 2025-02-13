// Copyright © by Jeff Foley 2017-2025. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package gleif

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/owasp-amass/amass/v4/engine/plugins/support"
	et "github.com/owasp-amass/amass/v4/engine/types"
	"github.com/owasp-amass/amass/v4/utils/net/http"
	dbt "github.com/owasp-amass/asset-db/types"
	oam "github.com/owasp-amass/open-asset-model"
	"github.com/owasp-amass/open-asset-model/contact"
	"github.com/owasp-amass/open-asset-model/general"
	"github.com/owasp-amass/open-asset-model/org"
)

type fuzzyCompletions struct {
	name   string
	plugin *gleif
}

func (fc *fuzzyCompletions) check(e *et.Event) error {
	_, ok := e.Entity.Asset.(*org.Organization)
	if !ok {
		return errors.New("failed to extract the Organization asset")
	}

	since, err := support.TTLStartTime(e.Session.Config(), string(oam.Organization), string(oam.Organization), fc.name)
	if err != nil {
		return err
	}

	var id *dbt.Entity
	if support.AssetMonitoredWithinTTL(e.Session, e.Entity, fc.plugin.source, since) {
		id = fc.lookup(e, e.Entity, fc.plugin.source, since)
	} else {
		id = fc.query(e, e.Entity, fc.plugin.source)
		support.MarkAssetMonitored(e.Session, e.Entity, fc.plugin.source)
	}

	if id != nil {
		fc.process(e, e.Entity, id)
	}
	return nil
}

func (fc *fuzzyCompletions) lookup(e *et.Event, o *dbt.Entity, src *et.Source, since time.Time) *dbt.Entity {
	var ids []*dbt.Entity

	if edges, err := e.Session.Cache().OutgoingEdges(o, since, "id"); err == nil {
		for _, edge := range edges {
			if tags, err := e.Session.Cache().GetEdgeTags(edge, since, src.Name); err != nil || len(tags) == 0 {
				continue
			}
			if a, err := e.Session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
				if _, ok := a.Asset.(*general.Identifier); ok {
					ids = append(ids, a)
				}
			}
		}
	}

	for _, ident := range ids {
		if id := ident.Asset.(*general.Identifier); id != nil && id.Type == general.LEICode {
			return ident
		}
	}
	return nil
}

func (fc *fuzzyCompletions) query(e *et.Event, orgent *dbt.Entity, src *et.Source) *dbt.Entity {
	var lei *general.Identifier

	if leient := fc.plugin.orgEntityToLEI(e, orgent); leient != nil {
		lei = leient.Asset.(*general.Identifier)
	}

	if lei == nil {
		fc.plugin.rlimit.Take()
		o := orgent.Asset.(*org.Organization)
		u := "https://api.gleif.org/api/v1/fuzzycompletions?field=fulltext&q=" + url.QueryEscape(o.Name)
		resp, err := http.RequestWebPage(context.TODO(), &http.Request{URL: u})
		if err != nil || resp.Body == "" {
			return nil
		}

		var result struct {
			Data []struct {
				Type       string `json:"type"`
				Attributes struct {
					Value string `json:"value"`
				} `json:"attributes"`
				Relationships struct {
					LEIRecords struct {
						Data struct {
							Type string `json:"type"`
							ID   string `json:"id"`
						} `json:"data"`
						Links struct {
							Related string `json:"related"`
						} `json:"links"`
					} `json:"lei-records"`
				} `json:"relationships"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(resp.Body), &result); err != nil || len(result.Data) == 0 {
			return nil
		}

		for _, d := range result.Data {
			if strings.EqualFold(d.Attributes.Value, o.Name) {
				lei = &general.Identifier{
					UniqueID: fmt.Sprintf("%s:%s", general.LEICode, d.Relationships.LEIRecords.Data.ID),
					EntityID: d.Relationships.LEIRecords.Data.ID,
					Type:     general.LEICode,
				}
				break
			}
		}
		if lei == nil {
			return nil
		}
	}

	rec, err := fc.plugin.getLEIRecord(e, lei)
	if err == nil && fc.locMatch(e, orgent, rec) {
		return nil
	}

	return fc.store(e, orgent, lei, rec)
}

func (fc *fuzzyCompletions) locMatch(e *et.Event, orgent *dbt.Entity, rec *leiRecord) bool {
	if edges, err := e.Session.Cache().OutgoingEdges(orgent, time.Time{}, "location"); err == nil {
		for _, edge := range edges {
			if a, err := e.Session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
				if loc, ok := a.Asset.(*contact.Location); ok &&
					(loc.PostalCode == rec.Attributes.Entity.LegalAddress.PostalCode ||
						(loc.PostalCode == rec.Attributes.Entity.HeadquartersAddress.PostalCode)) {
					return true
				}
			}
		}
	}

	var crs []*dbt.Entity
	if edges, err := e.Session.Cache().IncomingEdges(orgent, time.Time{}, "organization"); err == nil {
		for _, edge := range edges {
			if a, err := e.Session.Cache().FindEntityById(edge.FromEntity.ID); err == nil && a != nil {
				if _, ok := a.Asset.(*contact.ContactRecord); ok {
					crs = append(crs, a)
				}
			}
		}
	}

	for _, cr := range crs {
		if edges, err := e.Session.Cache().OutgoingEdges(cr, time.Time{}, "location"); err == nil {
			for _, edge := range edges {
				if a, err := e.Session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
					if loc, ok := a.Asset.(*contact.Location); ok &&
						(loc.PostalCode == rec.Attributes.Entity.LegalAddress.PostalCode ||
							(loc.PostalCode == rec.Attributes.Entity.HeadquartersAddress.PostalCode)) {
						return true
					}
				}
			}
		}
	}
	return false
}

func (fc *fuzzyCompletions) store(e *et.Event, orgent *dbt.Entity, id *general.Identifier, rec *leiRecord) *dbt.Entity {
	fc.plugin.updateOrgFromLEIRecord(e, orgent, rec)

	a, err := e.Session.Cache().CreateAsset(id)
	if err != nil || a == nil {
		e.Session.Log().Error(err.Error(), slog.Group("plugin", "name", fc.plugin.name, "handler", fc.name))
		return nil
	}

	_, _ = e.Session.Cache().CreateEntityProperty(a, &general.SourceProperty{
		Source:     fc.plugin.source.Name,
		Confidence: fc.plugin.source.Confidence,
	})

	edge, err := e.Session.Cache().CreateEdge(&dbt.Edge{
		Relation:   &general.SimpleRelation{Name: "id"},
		FromEntity: orgent,
		ToEntity:   a,
	})
	if err != nil && edge == nil {
		e.Session.Log().Error(err.Error(), slog.Group("plugin", "name", fc.plugin.name, "handler", fc.name))
		return nil
	}

	_, _ = e.Session.Cache().CreateEdgeProperty(edge, &general.SourceProperty{
		Source:     fc.plugin.source.Name,
		Confidence: fc.plugin.source.Confidence,
	})

	return a
}

func (fc *fuzzyCompletions) process(e *et.Event, orgent, ident *dbt.Entity) {
	id := ident.Asset.(*general.Identifier)

	_ = e.Dispatcher.DispatchEvent(&et.Event{
		Name:    id.UniqueID,
		Entity:  ident,
		Session: e.Session,
	})

	o := orgent.Asset.(*org.Organization)
	e.Session.Log().Info("relationship discovered", "from", o.Name, "relation", "id",
		"to", id.UniqueID, slog.Group("plugin", "name", fc.plugin.name, "handler", fc.name))
}
