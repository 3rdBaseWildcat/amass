// Copyright © by Jeff Foley 2017-2025. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package support

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"time"

	et "github.com/owasp-amass/amass/v4/engine/types"
	"github.com/owasp-amass/amass/v4/utils"
	dbt "github.com/owasp-amass/asset-db/types"
	oam "github.com/owasp-amass/open-asset-model"
	oamcert "github.com/owasp-amass/open-asset-model/certificate"
	"github.com/owasp-amass/open-asset-model/contact"
	oamdns "github.com/owasp-amass/open-asset-model/dns"
	"github.com/owasp-amass/open-asset-model/general"
	"github.com/owasp-amass/open-asset-model/org"
	"github.com/owasp-amass/open-asset-model/platform"
	oamreg "github.com/owasp-amass/open-asset-model/registration"
)

func SourceToAssetsWithinTTL(session et.Session, name, atype string, src *et.Source, since time.Time) []*dbt.Entity {
	var entities []*dbt.Entity

	switch atype {
	case string(oam.FQDN):
		roots, err := session.Cache().FindEntitiesByContent(&oamdns.FQDN{Name: name}, since)
		if err != nil || len(roots) != 1 {
			return nil
		}
		root := roots[0]

		entities, _ = utils.FindByFQDNScope(session.Cache(), root, since)
	case string(oam.Identifier):
		if parts := strings.Split(name, ":"); len(parts) == 2 {
			id := &general.Identifier{
				UniqueID: name,
				EntityID: parts[1],
				Type:     parts[0],
			}

			entities, _ = session.Cache().FindEntitiesByContent(id, since)
		}
	case string(oam.AutnumRecord):
		num, err := strconv.Atoi(name)
		if err != nil {
			return nil
		}

		entities, _ = session.Cache().FindEntitiesByContent(&oamreg.AutnumRecord{Number: num}, since)
	case string(oam.IPNetRecord):
		prefix, err := netip.ParsePrefix(name)
		if err != nil {
			return nil
		}

		entities, _ = session.Cache().FindEntitiesByContent(&oamreg.IPNetRecord{CIDR: prefix}, since)
	case string(oam.Service):
		entities, _ = session.Cache().FindEntitiesByContent(&platform.Service{ID: name}, since)
	}

	var results []*dbt.Entity
	for _, entity := range entities {
		if tags, err := session.Cache().GetEntityTags(entity, since, src.Name); err == nil && len(tags) > 0 {
			for _, tag := range tags {
				if tag.Property.PropertyType() == oam.SourceProperty {
					results = append(results, entity)
				}
			}
		}
	}
	return results
}

func StoreFQDNsWithSource(session et.Session, names []string, src *et.Source, plugin, handler string) []*dbt.Entity {
	var results []*dbt.Entity

	if len(names) == 0 || src == nil {
		return results
	}

	for _, name := range names {
		if a, err := session.Cache().CreateAsset(&oamdns.FQDN{Name: name}); err == nil && a != nil {
			results = append(results, a)
			_, _ = session.Cache().CreateEntityProperty(a, &general.SourceProperty{
				Source:     src.Name,
				Confidence: src.Confidence,
			})
		} else {
			session.Log().Error(err.Error(), slog.Group("plugin", "name", plugin, "handler", handler))
		}
	}

	return results
}

func StoreEmailsWithSource(session et.Session, emails []string, src *et.Source, plugin, handler string) []*dbt.Entity {
	var results []*dbt.Entity

	if len(emails) == 0 || src == nil {
		return results
	}

	for _, e := range emails {
		email := strings.ToLower(e)

		if a, err := session.Cache().CreateAsset(&general.Identifier{
			UniqueID: fmt.Sprintf("%s:%s", general.EmailAddress, email),
			EntityID: email,
			Type:     general.EmailAddress,
		}); err == nil && a != nil {
			results = append(results, a)
			_, _ = session.Cache().CreateEntityProperty(a, &general.SourceProperty{
				Source:     src.Name,
				Confidence: src.Confidence,
			})
		} else {
			session.Log().Error(err.Error(), slog.Group("plugin", "name", plugin, "handler", handler))
		}
	}

	return results
}

func MarkAssetMonitored(session et.Session, asset *dbt.Entity, src *et.Source) {
	if asset == nil || src == nil {
		return
	}

	if tags, err := session.Cache().GetEntityTags(asset, time.Time{}, "last_monitored"); err == nil && len(tags) > 0 {
		for _, tag := range tags {
			if tag.Property.Value() == src.Name {
				_ = session.Cache().DeleteEntityTag(tag.ID)
			}
		}
	}

	_, _ = session.Cache().CreateEntityProperty(asset, general.SimpleProperty{
		PropertyName:  "last_monitored",
		PropertyValue: src.Name,
	})
}

func AssetMonitoredWithinTTL(session et.Session, asset *dbt.Entity, src *et.Source, since time.Time) bool {
	if asset == nil || src == nil || !since.IsZero() {
		return false
	}

	if tags, err := session.Cache().GetEntityTags(asset, since, "last_monitored"); err == nil && len(tags) > 0 {
		for _, tag := range tags {
			if tag.Property.Value() == src.Name {
				return true
			}
		}
	}

	return false
}

func CreateServiceAsset(session et.Session, src *dbt.Entity, rel oam.Relation, serv *platform.Service, cert *oamcert.TLSCertificate) (*dbt.Entity, error) {
	var result *dbt.Entity

	var srvs []*dbt.Entity
	if entities, err := session.Cache().FindEntitiesByType(oam.Service, time.Time{}); err == nil {
		for _, a := range entities {
			if s, ok := a.Asset.(*platform.Service); ok && s.OutputLen == serv.OutputLen {
				srvs = append(srvs, a)
			}
		}
	}

	var match *dbt.Entity
	for _, srv := range srvs {
		var num int

		s := srv.Asset.(*platform.Service)
		for _, key := range []string{"Server", "X-Powered-By"} {
			if server1, ok := serv.Attributes[key]; ok && server1[0] != "" {
				if server2, ok := s.Attributes[key]; ok && server1[0] == server2[0] {
					num++
				} else {
					num--
				}
			}
		}

		if cert != nil {
			if edges, err := session.Cache().OutgoingEdges(srv, time.Time{}, "certificate"); err == nil && len(edges) > 0 {
				var found bool

				for _, edge := range edges {
					if t, err := session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && t != nil {
						if c, ok := t.Asset.(*oamcert.TLSCertificate); ok && c.SerialNumber == cert.SerialNumber {
							found = true
							break
						}
					}
				}

				if found {
					num++
				} else {
					continue
				}
			}
		}

		if num > 0 {
			match = srv
			break
		}
	}

	if match != nil {
		result = match
	} else {
		if a, err := session.Cache().CreateAsset(serv); err == nil && a != nil {
			result = a
		} else {
			return nil, errors.New("failed to create the OAM Service asset")
		}
	}

	_, err := session.Cache().CreateEdge(&dbt.Edge{
		Relation:   rel,
		FromEntity: src,
		ToEntity:   result,
	})
	return result, err
}

func CreateOrgAsset(session et.Session, obj *dbt.Entity, rel oam.Relation, o *org.Organization, src *et.Source) (*dbt.Entity, error) {
	if orgent := orgDedupChecks(session, obj, o); orgent != nil {
		if err := createRelation(session, obj, rel, orgent, src); err != nil {
			return nil, err
		}
		return orgent, nil
	}

	id := &general.Identifier{
		UniqueID: fmt.Sprintf("%s:%s", "org_name", o.Name),
		EntityID: o.Name,
		Type:     "org_name",
	}

	if ident, err := session.Cache().CreateAsset(id); err == nil && ident != nil {
		if orgent, err := session.Cache().CreateAsset(o); err == nil && orgent != nil {
			if err := createRelation(session, orgent, &general.SimpleRelation{Name: "id"}, ident, src); err != nil {
				return nil, err
			}
			if err := createRelation(session, obj, rel, orgent, src); err != nil {
				return nil, err
			}
			return orgent, nil
		}
	}

	return nil, errors.New("failed to create the OAM Organization asset")
}

func orgDedupChecks(session et.Session, obj *dbt.Entity, o *org.Organization) *dbt.Entity {
	var result *dbt.Entity

	switch obj.Asset.(type) {
	case *contact.ContactRecord:
		if org, found := orgNameExistsInContactRecord(session, obj, o.Name); found {
			result = org
		}
		if org, err := orgExistsAndSharesLocEntity(session, obj, o); err == nil {
			result = org
		}
		if org, err := orgExistsAndSharesAncestorEntity(session, obj, o); err == nil {
			result = org
		}
	case *org.Organization:
		if org, err := orgExistsAndSharesLocEntity(session, obj, o); err == nil {
			result = org
		}
		if org, err := orgExistsAndSharesAncestorEntity(session, obj, o); err == nil {
			result = org
		}
	}

	return result
}

func orgHasName(session et.Session, org *dbt.Entity, name string) bool {
	if org == nil {
		return false
	}

	if edges, err := session.Cache().OutgoingEdges(org, time.Time{}, "id"); err == nil && len(edges) > 0 {
		for _, edge := range edges {
			if a, err := session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
				if id, ok := a.Asset.(*general.Identifier); ok && strings.EqualFold(id.EntityID, name) {
					return true
				}
			}
		}
	}
	return false
}

func orgNameExistsInContactRecord(session et.Session, cr *dbt.Entity, name string) (*dbt.Entity, bool) {
	if cr == nil {
		return nil, false
	}

	if edges, err := session.Cache().OutgoingEdges(cr, time.Time{}, "organization"); err == nil && len(edges) > 0 {
		for _, edge := range edges {
			if a, err := session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
				if _, ok := a.Asset.(*org.Organization); ok && orgHasName(session, a, name) {
					return a, true
				}
			}
		}
	}
	return nil, false
}

func orgExistsAndSharesLocEntity(session et.Session, obj *dbt.Entity, o *org.Organization) (*dbt.Entity, error) {
	var locs []*dbt.Entity

	if edges, err := session.Cache().OutgoingEdges(obj, time.Time{}, "location"); err == nil {
		for _, edge := range edges {
			if a, err := session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
				if _, ok := a.Asset.(*contact.Location); ok {
					locs = append(locs, a)
				}
			}
		}
	}

	var orgents, crecords []*dbt.Entity
	for _, loc := range locs {
		if edges, err := session.Cache().IncomingEdges(loc, time.Time{}, "location"); err == nil {
			for _, edge := range edges {
				if a, err := session.Cache().FindEntityById(edge.FromEntity.ID); err == nil && a != nil {
					if _, ok := a.Asset.(*contact.ContactRecord); ok && a.ID != obj.ID {
						crecords = append(crecords, a)
					} else if _, ok := a.Asset.(*org.Organization); ok {
						orgents = append(orgents, a)
					}
				}
			}
		}
	}

	for _, cr := range crecords {
		if edges, err := session.Cache().OutgoingEdges(cr, time.Time{}, "organization"); err == nil {
			for _, edge := range edges {
				if a, err := session.Cache().FindEntityById(edge.ToEntity.ID); err == nil && a != nil {
					if _, ok := a.Asset.(*org.Organization); ok {
						orgents = append(orgents, a)
					}
				}
			}
		}
	}

	for _, orgent := range orgents {
		if strings.EqualFold(orgent.Asset.(*org.Organization).Name, o.Name) {
			return orgent, nil
		}
	}

	return nil, errors.New("no matching org found")
}

func orgExistsAndSharesAncestorEntity(session et.Session, obj *dbt.Entity, o *org.Organization) (*dbt.Entity, error) {
	var idents []*dbt.Entity

	if assets, err := session.Cache().FindEntitiesByContent(&general.Identifier{
		UniqueID: fmt.Sprintf("%s:%s", "org_name", o.Name),
		EntityID: o.Name,
		Type:     "org_name",
	}, time.Time{}); err == nil {
		for _, a := range assets {
			if _, ok := a.Asset.(*general.Identifier); ok {
				idents = append(idents, a)
			}
		}
	}

	var orgents []*dbt.Entity
	for _, ident := range idents {
		if edges, err := session.Cache().IncomingEdges(ident, time.Time{}, "id"); err == nil {
			for _, edge := range edges {
				if a, err := session.Cache().FindEntityById(edge.FromEntity.ID); err == nil && a != nil {
					if _, ok := a.Asset.(*org.Organization); ok {
						orgents = append(orgents, a)
					}
				}
			}
		}
	}
	if len(orgents) == 0 {
		return nil, errors.New("no matching org found")
	}

	assets := []*dbt.Entity{obj}
	ancestors := make(map[string]struct{})
	for len(assets) > 0 {
		remaining := assets
		assets = []*dbt.Entity{}

		for _, r := range remaining {
			if edges, err := session.Cache().IncomingEdges(r, time.Time{}); err == nil {
				for _, edge := range edges {
					if a, err := session.Cache().FindEntityById(edge.FromEntity.ID); err == nil && a != nil {
						if _, found := ancestors[a.ID]; !found {
							if _, conf := session.Scope().IsAssetInScope(a.Asset, 0); conf > 0 {
								ancestors[a.ID] = struct{}{}
								assets = append(assets, a)
							}
						}
					}
				}
			}
		}
	}

	assets = orgents
	for len(assets) > 0 {
		remaining := assets
		assets = []*dbt.Entity{}

		for _, orgent := range remaining {
			if edges, err := session.Cache().IncomingEdges(orgent, time.Time{}); err == nil {
				for _, edge := range edges {
					if a, err := session.Cache().FindEntityById(edge.FromEntity.ID); err == nil && a != nil {
						if _, found := ancestors[a.ID]; !found {
							if _, conf := session.Scope().IsAssetInScope(a.Asset, 0); conf > 0 {
								ancestors[a.ID] = struct{}{}
								assets = append(assets, a)
							}
						} else {
							return orgent, nil
						}
					}
				}
			}
		}
	}

	return nil, errors.New("no matching org found")
}

func createRelation(session et.Session, obj *dbt.Entity, rel oam.Relation, subject *dbt.Entity, src *et.Source) error {
	edge, err := session.Cache().CreateEdge(&dbt.Edge{
		Relation:   rel,
		FromEntity: obj,
		ToEntity:   subject,
	})
	if err != nil {
		return err
	} else if edge == nil {
		return errors.New("failed to create the edge")
	}

	_, err = session.Cache().CreateEdgeProperty(edge, &general.SourceProperty{
		Source:     src.Name,
		Confidence: src.Confidence,
	})
	return err
}
