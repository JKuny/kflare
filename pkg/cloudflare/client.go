/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"net/http"
	"slices"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/option"
)

// Client is a thin wrapper around the cloudflare-go *API that serves as the
// single production implementation for all kflare controllers.
//
// Testability is achieved through narrow per-controller interfaces: each
// controller declares only the methods it uses, and tests inject a fake that
// satisfies those methods.  *Client satisfies every such interface because it
// embeds *cf.API, which provides all upstream Cloudflare SDK methods.
//
// It also carries what cloudflare-go v7, the SDK Cloudflare generates from
// its OpenAPI spec, needs to call the API with the same token: see V7Options.
//
// Error handling is intentionally left to the caller; use IsTerminalError,
// IsNotFound, and IsRateLimit from this package to classify errors returned
// by any method of either SDK.
type Client struct {
	*cf.API

	v7 []option.RequestOption
}

// New constructs a Client from a raw Cloudflare API token.
//
// The token is validated for basic format requirements by the cloudflare-go
// SDK.  An error is returned if the token string is empty or malformed; note
// that credential correctness (i.e. whether the token is accepted by the
// Cloudflare API) is only confirmed when the first API call is made.
func New(token string) (*Client, error) {
	api, err := cf.NewWithAPIToken(token)
	if err != nil {
		return nil, err
	}
	return &Client{
		API: api,
		v7: []option.RequestOption{
			option.WithHTTPClient(http.DefaultClient),
			option.WithEnvironmentProduction(),
			option.WithAPIToken(token),
		},
	}, nil
}

// V7Options returns the request options for the cloudflare-go v7 services a
// controller calls with this client's token, for example
//
//	namespaces := kv.NewNamespaceService(client.V7Options()...)
//
// kflare builds only the services it calls and never imports v7's root
// package, which golangci-lint's depguard refuses. Its NewClient builds a
// service for every Cloudflare product and applies six CLOUDFLARE_*
// environment variables to every request: other credentials, another base
// URL and extra headers, whereas kflare takes each account's token from its
// Secret. Importing the package at all pulls every v7 package into the
// manager, which grows by a third. A param field is set the way its F would
// set it:
//
//	params.Title.Value, params.Title.Present = title, true
//
// The services share http.DefaultClient, as v0 does, so that connections are
// reused across reconciles. They keep v7's retries: two, after a 408, 409,
// 429, 5xx or connection error.
func (c *Client) V7Options() []option.RequestOption {
	return slices.Clone(c.v7)
}
