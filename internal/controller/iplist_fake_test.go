/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/gomega"
)

const ipListTestAccount = "acct-iplist"

// fakeIPListAPI is an in-memory Lists API for one account. It follows the
// behavior verified against the live API on 2026-10-13:
//
//   - List names are unique in the account across list kinds (400, code
//     10021), and the plan caps how many lists of a kind an account has (400,
//     code 10019). Only a list's description can change.
//   - Replacing a list's items checks them all first and refuses the whole
//     request (400) for an address that does not parse (10037), a range with
//     host bits set (10036), an IPv4 range outside /8 to /32 (10034) or an
//     IPv6 one outside /12 to /128 (10035), an IPv4-mapped IPv6 address
//     (10080), one address twice, in any form (10028), a comment over 500
//     characters (10022), or more items than the account may hold (10020).
//   - The replace itself is a bulk operation, which this fake reports as
//     running for `polls` checks before it completes and applies the items.
//     An unknown operation is a 404 (10001).
//   - Cloudflare stores an address in canonical form: a range of one address
//     as that address, IPv6 in lowercase with zeros compressed. It returns
//     items in no particular order (this fake: reversed), and an empty comment
//     as null.
//   - Deleting a list that a rule uses is refused (400, 10041); deleting an
//     unknown list succeeds. An unknown list is otherwise a 404 (10001).
type fakeIPListAPI struct {
	lists      map[string]*cf.List
	items      map[string][]cf.ListItem // by list ID
	operations map[string]*fakeListOperation
	maxLists   int
	maxItems   int
	polls      int             // checks a bulk operation reports running for
	inUse      map[string]bool // IDs of lists a rule uses
	nextID     int

	// before runs at the start of every call with the method's name.
	before func(method string)
	// errs makes a method fail with the error instead of running.
	errs map[string]error
	// failOperations makes every bulk operation fail with this message.
	failOperations string

	writes   int                          // successful creates, updates, deletes and replaces
	replaced [][]cf.ListItemCreateRequest // the items of every replace, as sent
}

// fakeListOperation is a bulk operation replacing a list's items.
type fakeListOperation struct {
	listID string
	items  []cf.ListItem
	checks int
}

func newFakeIPListAPI() *fakeIPListAPI {
	return &fakeIPListAPI{
		lists:      map[string]*cf.List{},
		items:      map[string][]cf.ListItem{},
		operations: map[string]*fakeListOperation{},
		maxLists:   1,
		maxItems:   10000,
		inUse:      map[string]bool{},
		errs:       map[string]error{},
	}
}

func (f *fakeIPListAPI) start(method string, rc *cf.ResourceContainer) error {
	if rc == nil || rc.Level != cf.AccountRouteLevel || rc.Identifier != ipListTestAccount {
		return fmt.Errorf("fake: %s called for %+v, want account %s", method, rc, ipListTestAccount)
	}
	if f.before != nil {
		f.before(method)
	}
	return f.errs[method]
}

func (f *fakeIPListAPI) newID() string {
	f.nextID++
	return fmt.Sprintf("list%02d", f.nextID)
}

// seed gives the account a list holding items, stored as Cloudflare stores
// them, and returns its ID.
func (f *fakeIPListAPI) seed(name, kind, description string, items ...cf.ListItem) string {
	id := f.newID()
	f.lists[id] = &cf.List{ID: id, Name: name, Kind: kind, Description: description, NumItems: len(items)}
	f.items[id] = items
	return id
}

// listNamed returns the fake's list named name, or nil.
func (f *fakeIPListAPI) listNamed(name string) *cf.List {
	for _, list := range f.lists {
		if list.Name == name {
			return list
		}
	}
	return nil
}

// storedItems returns the items of the account's one list as "ip comment"
// strings.
func (f *fakeIPListAPI) storedItems() []string {
	Expect(f.lists).To(HaveLen(1), "the account has one list")
	var out []string
	for id := range f.lists {
		for _, item := range f.items[id] {
			out = append(out, strings.TrimSpace(*item.IP+" "+item.Comment))
		}
	}
	return out
}

func (f *fakeIPListAPI) ListLists(_ context.Context, rc *cf.ResourceContainer, _ cf.ListListsParams) ([]cf.List, error) {
	if err := f.start("ListLists", rc); err != nil {
		return nil, err
	}
	var lists []cf.List
	for _, list := range f.lists {
		lists = append(lists, *list)
	}
	return lists, nil
}

func (f *fakeIPListAPI) CreateList(_ context.Context, rc *cf.ResourceContainer, params cf.ListCreateParams) (cf.List, error) {
	if err := f.start("CreateList", rc); err != nil {
		return cf.List{}, err
	}
	if f.listNamed(params.Name) != nil {
		return cf.List{}, cloudflareError(400, 10021)
	}
	count := 0
	for _, list := range f.lists {
		if list.Kind == params.Kind {
			count++
		}
	}
	if count >= f.maxLists {
		return cf.List{}, cloudflareError(400, 10019)
	}
	f.writes++
	id := f.seed(params.Name, params.Kind, params.Description)
	return *f.lists[id], nil
}

func (f *fakeIPListAPI) UpdateList(_ context.Context, rc *cf.ResourceContainer, params cf.ListUpdateParams) (cf.List, error) {
	if err := f.start("UpdateList", rc); err != nil {
		return cf.List{}, err
	}
	list, ok := f.lists[params.ID]
	if !ok {
		return cf.List{}, cloudflareError(404, 10001)
	}
	f.writes++
	list.Description = params.Description
	return *list, nil
}

func (f *fakeIPListAPI) DeleteList(_ context.Context, rc *cf.ResourceContainer, listID string) (cf.ListDeleteResponse, error) {
	if err := f.start("DeleteList", rc); err != nil {
		return cf.ListDeleteResponse{}, err
	}
	if f.inUse[listID] {
		return cf.ListDeleteResponse{}, cloudflareError(400, ipListInUseCode)
	}
	if _, ok := f.lists[listID]; ok {
		f.writes++
		delete(f.lists, listID)
		delete(f.items, listID)
	}
	response := cf.ListDeleteResponse{}
	response.Result.ID = listID
	return response, nil
}

func (f *fakeIPListAPI) ListListItems(_ context.Context, rc *cf.ResourceContainer, params cf.ListListItemsParams) ([]cf.ListItem, error) {
	if err := f.start("ListListItems", rc); err != nil {
		return nil, err
	}
	if _, ok := f.lists[params.ID]; !ok {
		return nil, cloudflareError(404, 10001)
	}
	if params.PerPage > 500 {
		return nil, cloudflareError(400, 10027) // what Cloudflare answers to a page over 500
	}
	stored := f.items[params.ID]
	items := make([]cf.ListItem, 0, len(stored))
	for i := len(stored) - 1; i >= 0; i-- {
		items = append(items, stored[i])
	}
	return items, nil
}

func (f *fakeIPListAPI) ReplaceListItemsAsync(
	_ context.Context, rc *cf.ResourceContainer, params cf.ListReplaceItemsParams,
) (cf.ListItemCreateResponse, error) {
	if err := f.start("ReplaceListItemsAsync", rc); err != nil {
		return cf.ListItemCreateResponse{}, err
	}
	list, ok := f.lists[params.ID]
	if !ok {
		return cf.ListItemCreateResponse{}, cloudflareError(404, 10001)
	}
	total := len(params.Items)
	for id, items := range f.items {
		if id != list.ID {
			total += len(items)
		}
	}
	if total > f.maxItems {
		return cf.ListItemCreateResponse{}, cloudflareError(400, 10020)
	}
	stored := make([]cf.ListItem, 0, len(params.Items))
	seen := map[string]bool{}
	for _, item := range params.Items {
		ip, code := storedIP(*item.IP)
		switch {
		case code != 0:
			return cf.ListItemCreateResponse{}, cloudflareError(400, code)
		case seen[ip]:
			return cf.ListItemCreateResponse{}, cloudflareError(400, 10028)
		case utf8.RuneCountInString(item.Comment) > 500:
			return cf.ListItemCreateResponse{}, cloudflareError(400, 10022)
		}
		seen[ip] = true
		stored = append(stored, cf.ListItem{ID: f.newID(), IP: &ip, Comment: item.Comment})
	}
	f.writes++
	f.replaced = append(f.replaced, params.Items)
	id := f.newID()
	f.operations[id] = &fakeListOperation{listID: list.ID, items: stored}
	response := cf.ListItemCreateResponse{}
	response.Result.OperationID = id
	return response, nil
}

func (f *fakeIPListAPI) GetListBulkOperation(_ context.Context, rc *cf.ResourceContainer, id string) (cf.ListBulkOperation, error) {
	if err := f.start("GetListBulkOperation", rc); err != nil {
		return cf.ListBulkOperation{}, err
	}
	operation, ok := f.operations[id]
	if !ok {
		return cf.ListBulkOperation{}, cloudflareError(404, 10001)
	}
	if operation.checks < f.polls {
		operation.checks++
		return cf.ListBulkOperation{ID: id, Status: "running"}, nil
	}
	if f.failOperations != "" {
		return cf.ListBulkOperation{ID: id, Status: "failed", Error: f.failOperations}, nil
	}
	if list, ok := f.lists[operation.listID]; ok {
		f.items[list.ID] = operation.items
		list.NumItems = len(operation.items)
	}
	return cf.ListBulkOperation{ID: id, Status: "completed"}, nil
}

// storedIP checks value as Cloudflare does and returns the form it stores it
// in, or the error code it refuses it with.
func storedIP(value string) (string, int) {
	if prefix, err := netip.ParsePrefix(value); err == nil {
		addr := prefix.Addr()
		switch {
		case addr.Zone() != "":
			return "", 10037
		case addr.Is4In6():
			return "", 10080
		case prefix != prefix.Masked():
			return "", 10036
		case addr.Is4() && prefix.Bits() < 8:
			return "", 10034
		case addr.Is6() && prefix.Bits() < 12:
			return "", 10035
		case prefix.IsSingleIP():
			return addr.String(), 0
		}
		return prefix.String(), 0
	}
	addr, err := netip.ParseAddr(value)
	switch {
	case err != nil || addr.Zone() != "":
		return "", 10037
	case addr.Is4In6():
		return "", 10080
	}
	return addr.String(), 0
}
