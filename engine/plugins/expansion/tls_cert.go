// Copyright © by Jeff Foley 2017-2024. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package expansion

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/owasp-amass/amass/v4/config"
	"github.com/owasp-amass/amass/v4/engine/plugins/support"
	et "github.com/owasp-amass/amass/v4/engine/types"
	dbt "github.com/owasp-amass/asset-db/types"
	oam "github.com/owasp-amass/open-asset-model"
	oamcert "github.com/owasp-amass/open-asset-model/certificate"
	"github.com/owasp-amass/open-asset-model/contact"
	"github.com/owasp-amass/open-asset-model/domain"
	"github.com/owasp-amass/open-asset-model/network"
	"github.com/owasp-amass/open-asset-model/org"
	"github.com/owasp-amass/open-asset-model/property"
	"github.com/owasp-amass/open-asset-model/relation"
)

type tlsexpand struct {
	name       string
	log        *slog.Logger
	transforms []string
	source     *et.Source
}

func NewTLSCerts() et.Plugin {
	return &tlsexpand{
		name: "TLCert-Expansion",
		transforms: []string{
			string(oam.URL),
			string(oam.FQDN),
			string(oam.IPAddress),
			string(oam.ContactRecord),
			string(oam.Organization),
			string(oam.Location),
			string(oam.EmailAddress),
			string(oam.TLSCertificate),
		},
		source: &et.Source{
			Name:       "TLCert-Expansion",
			Confidence: 100,
		},
	}
}

func (te *tlsexpand) Name() string {
	return te.name
}

func (te *tlsexpand) Start(r et.Registry) error {
	te.log = r.Log().WithGroup("plugin").With("name", te.name)

	if err := r.RegisterHandler(&et.Handler{
		Plugin:     te,
		Name:       te.name,
		Transforms: te.transforms,
		EventType:  oam.TLSCertificate,
		Callback:   te.check,
	}); err != nil {
		return err
	}

	te.log.Info("Plugin started")
	return nil
}

func (te *tlsexpand) Stop() {
	te.log.Info("Plugin stopped")
}

func (te *tlsexpand) check(e *et.Event) error {
	_, ok := e.Entity.Asset.(*oamcert.TLSCertificate)
	if !ok {
		return errors.New("failed to extract the TLSCertificate asset")
	}

	matches, err := e.Session.Config().CheckTransformations(
		string(oam.TLSCertificate), append(te.transforms, te.name)...)
	if err != nil || matches.Len() == 0 {
		return nil
	}

	var findings []*support.Finding
	if cert, ok := e.Meta.(*x509.Certificate); ok && cert != nil {
		findings = append(findings, te.store(e, cert, e.Entity, te.source, matches)...)
	} else {
		findings = append(findings, te.lookup(e, e.Entity, te.source, matches)...)
	}

	if len(findings) > 0 {
		te.process(e, findings, te.source)
	}
	return nil
}

func (te *tlsexpand) lookup(e *et.Event, asset *dbt.Entity, src *et.Source, m *config.Matches) []*support.Finding {
	var rtypes []string
	var findings []*support.Finding
	sinces := make(map[string]time.Time)

	for _, atype := range te.transforms {
		if !m.IsMatch(atype) {
			continue
		}

		since, err := support.TTLStartTime(e.Session.Config(), string(oam.TLSCertificate), atype, te.name)
		if err != nil {
			continue
		}
		sinces[atype] = since

		switch atype {
		case string(oam.URL):
			rtypes = append(rtypes, "san_url", "ocsp_server", "issuing_certificate_url")
		case string(oam.FQDN):
			rtypes = append(rtypes, "common_name", "san_dns_name")
		case string(oam.ContactRecord):
			rtypes = append(rtypes, "subject_contact", "issuer_contact")
		case string(oam.IPAddress):
			rtypes = append(rtypes, "san_ip_address")
		case string(oam.EmailAddress):
			rtypes = append(rtypes, "san_email_address")
		case string(oam.TLSCertificate):
			rtypes = append(rtypes, "issuing_certificate")
		}
	}

	if edges, err := e.Session.Cache().OutgoingEdges(asset, time.Time{}, rtypes...); err == nil && len(edges) > 0 {
		for _, edge := range edges {
			a, err := e.Session.Cache().FindEntityById(edge.ToEntity.ID)
			if err != nil {
				continue
			}
			totype := string(a.Asset.AssetType())

			since, ok := sinces[totype]
			if !ok || (ok && a.LastSeen.Before(since)) {
				continue
			}

			if !te.oneOfSources(e, edge, te.source, since) {
				continue
			}

			t := asset.Asset.(*oamcert.TLSCertificate)
			findings = append(findings, &support.Finding{
				From:     asset,
				FromName: "TLSCertificate: " + t.SerialNumber,
				To:       a,
				ToName:   a.Asset.Key(),
				Rel:      edge.Relation,
			})
		}
	}

	return findings
}

func (te *tlsexpand) oneOfSources(e *et.Event, edge *dbt.Edge, src *et.Source, since time.Time) bool {
	if tags, err := e.Session.Cache().GetEdgeTags(edge, since, src.Name); err == nil && len(tags) > 0 {
		for _, tag := range tags {
			if _, ok := tag.Property.(*property.SourceProperty); ok {
				return true
			}
		}
	}
	return false
}

func (te *tlsexpand) store(e *et.Event, cert *x509.Certificate, asset *dbt.Entity, src *et.Source, m *config.Matches) []*support.Finding {
	var findings []*support.Finding
	t := asset.Asset.(*oamcert.TLSCertificate)

	if m.IsMatch(string(oam.FQDN)) {
		if common := t.SubjectCommonName; common != "" {
			if a, err := e.Session.Cache().CreateAsset(&domain.FQDN{Name: common}); err == nil && a != nil {
				findings = append(findings, &support.Finding{
					From:     asset,
					FromName: "TLSCertificate: " + t.SerialNumber,
					To:       a,
					ToName:   common,
					Rel:      &relation.SimpleRelation{Name: "common_name"},
				})
			}
		}
		for _, n := range cert.DNSNames {
			for _, name := range support.ScrapeSubdomainNames(strings.ToLower(strings.TrimSpace(n))) {
				if a, err := e.Session.Cache().CreateAsset(&domain.FQDN{Name: name}); err == nil && a != nil {
					findings = append(findings, &support.Finding{
						From:     asset,
						FromName: "TLSCertificate: " + t.SerialNumber,
						To:       a,
						ToName:   name,
						Rel:      &relation.SimpleRelation{Name: "san_dns_name"},
					})
				}
			}
		}
	}

	if m.IsMatch(string(oam.EmailAddress)) {
		for _, emailstr := range cert.EmailAddresses {
			if email := support.EmailToOAMEmailAddress(strings.ToLower(strings.TrimSpace(emailstr))); email != nil {
				if a, err := e.Session.Cache().CreateAsset(email); err == nil && a != nil {
					findings = append(findings, &support.Finding{
						From:     asset,
						FromName: "TLSCertificate: " + t.SerialNumber,
						To:       a,
						ToName:   email.Address,
						Rel:      &relation.SimpleRelation{Name: "san_email_address"},
					})
				}
			}
		}
	}

	if m.IsMatch(string(oam.IPAddress)) {
		for _, ip := range cert.IPAddresses {
			oamip := &network.IPAddress{Address: netip.MustParseAddr(ip.String())}

			if oamip.Address.Is4() {
				oamip.Type = "IPv4"
			} else {
				oamip.Type = "IPv6"
			}

			if a, err := e.Session.Cache().CreateAsset(oamip); err == nil && a != nil {
				findings = append(findings, &support.Finding{
					From:     asset,
					FromName: "TLSCertificate: " + t.SerialNumber,
					To:       a,
					ToName:   oamip.Address.String(),
					Rel:      &relation.SimpleRelation{Name: "san_ip_address"},
				})
			}
		}
	}

	if m.IsMatch(string(oam.URL)) {
		for _, u := range cert.URIs {
			if oamurl := support.RawURLToOAM(u.String()); oamurl != nil {
				if a, err := e.Session.Cache().CreateAsset(oamurl); err == nil && a != nil {
					findings = append(findings, &support.Finding{
						From:     asset,
						FromName: "TLSCertificate: " + t.SerialNumber,
						To:       a,
						ToName:   oamurl.Raw,
						Rel:      &relation.SimpleRelation{Name: "san_url"},
					})
				}
			}
		}
		for _, u := range cert.IssuingCertificateURL {
			if oamurl := support.RawURLToOAM(u); oamurl != nil {
				if a, err := e.Session.Cache().CreateAsset(oamurl); err == nil && a != nil {
					findings = append(findings, &support.Finding{
						From:     asset,
						FromName: "TLSCertificate: " + t.SerialNumber,
						To:       a,
						ToName:   oamurl.Raw,
						Rel:      &relation.SimpleRelation{Name: "issuing_certificate_url"},
					})
				}
			}
		}
		for _, u := range cert.OCSPServer {
			if oamurl := support.RawURLToOAM(u); oamurl != nil {
				if a, err := e.Session.Cache().CreateAsset(oamurl); err == nil && a != nil {
					findings = append(findings, &support.Finding{
						From:     asset,
						FromName: "TLSCertificate: " + t.SerialNumber,
						To:       a,
						ToName:   oamurl.Raw,
						Rel:      &relation.SimpleRelation{Name: "ocsp_server"},
					})
				}
			}
		}
	}

	if !m.IsMatch(string(oam.ContactRecord)) {
		return findings
	}

	base := "x509 Certificate: " + cert.SerialNumber.String() + ", "
	contacts := []*tlsContact{
		{&cert.Subject, "subject_contact", base + "Subject"},
		{&cert.Issuer, "issuer_contact", base + "Issuer"},
	}
	for _, c := range contacts {
		findings = append(findings, te.storeContact(e, c, asset, te.source, m)...)
	}
	return findings
}

type tlsContact struct {
	contact      *pkix.Name
	RelationName string
	DiscoveredAt string
}

func (te *tlsexpand) storeContact(e *et.Event, c *tlsContact, asset *dbt.Entity, src *et.Source, m *config.Matches) []*support.Finding {
	ct := c.contact
	var findings []*support.Finding

	var foundaddr bool
	if len(ct.Province) > 0 && len(ct.Country) > 0 {
		foundaddr = true
	}

	var foundorgs bool
	if len(ct.Organization) > 0 {
		foundorgs = true
	}
	// only continue with the database operations if there's a contact record to create
	if !foundaddr && !foundorgs {
		return findings
	}

	cr, err := e.Session.Cache().CreateAsset(&contact.ContactRecord{DiscoveredAt: c.DiscoveredAt})
	if err != nil || cr == nil {
		return findings
	}

	t := asset.Asset.(*oamcert.TLSCertificate)
	findings = append(findings, &support.Finding{
		From:     asset,
		FromName: "TLSCertificate: " + t.SerialNumber,
		To:       cr,
		ToName:   "ContactRecord" + c.DiscoveredAt,
		Rel:      &relation.SimpleRelation{Name: c.RelationName},
	})

	if foundaddr && m.IsMatch(string(oam.Location)) {
		var addr string
		fields := [][]string{
			ct.Organization,
			ct.StreetAddress,
			ct.Locality,
			ct.Province,
			ct.PostalCode,
			ct.Country,
		}
		for _, field := range fields {
			if len(field) > 0 && field[0] != "" {
				addr += " " + field[0]
			}
		}
		if loc := support.StreetAddressToLocation(strings.TrimSpace(addr)); loc != nil {
			if a, err := e.Session.Cache().CreateAsset(loc); err == nil && a != nil {
				if edge, err := e.Session.Cache().CreateEdge(&dbt.Edge{
					Relation:   &relation.SimpleRelation{Name: "location"},
					FromEntity: cr,
					ToEntity:   a,
				}); err == nil && edge != nil {
					_, _ = e.Session.Cache().CreateEdgeProperty(edge, &property.SourceProperty{
						Source:     src.Name,
						Confidence: src.Confidence,
					})
				}
			}
		}
	}
	if len(ct.Organization) > 0 && ct.Organization[0] != "" && m.IsMatch(string(oam.Organization)) {
		if a, err := e.Session.Cache().CreateAsset(&org.Organization{Name: ct.Organization[0]}); err == nil && a != nil {
			if edge, err := e.Session.Cache().CreateEdge(&dbt.Edge{
				Relation:   &relation.SimpleRelation{Name: "organization"},
				FromEntity: cr,
				ToEntity:   a,
			}); err == nil && edge != nil {
				_, _ = e.Session.Cache().CreateEdgeProperty(edge, &property.SourceProperty{
					Source:     src.Name,
					Confidence: src.Confidence,
				})
			}
		}
	}
	if len(ct.OrganizationalUnit) > 0 && ct.OrganizationalUnit[0] != "" && m.IsMatch(string(oam.URL)) {
		if u := support.ExtractURLFromString(ct.OrganizationalUnit[0]); u != nil {
			if a, err := e.Session.Cache().CreateAsset(u); err == nil && a != nil {
				if edge, err := e.Session.Cache().CreateEdge(&dbt.Edge{
					Relation:   &relation.SimpleRelation{Name: "url"},
					FromEntity: cr,
					ToEntity:   a,
				}); err == nil && edge != nil {
					_, _ = e.Session.Cache().CreateEdgeProperty(edge, &property.SourceProperty{
						Source:     src.Name,
						Confidence: src.Confidence,
					})
				}
			}
		}
	}

	return findings
}

func (te *tlsexpand) process(e *et.Event, findings []*support.Finding, src *et.Source) {
	support.ProcessAssetsWithSource(e, findings, te.source, te.name, te.name+"-Handler")
}
