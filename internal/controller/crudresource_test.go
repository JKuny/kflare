/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"errors"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("findInPages", func() {
	// pages serves the pages of a list, each reporting perPage as its size,
	// and records which pages were asked for.
	pages := func(perPage int64, contents ...[]string) (func(int64) (*pagination.V4PagePaginationArray[string], error), *[]int64) {
		asked := &[]int64{}
		return func(page int64) (*pagination.V4PagePaginationArray[string], error) {
			*asked = append(*asked, page)
			res := &pagination.V4PagePaginationArray[string]{
				ResultInfo: pagination.V4PagePaginationArrayResultInfo{Page: page, PerPage: perPage},
			}
			if int(page) <= len(contents) {
				res.Result = contents[page-1]
			}
			return res, nil
		}, asked
	}
	is := func(want string) func(*string) bool { return func(s *string) bool { return *s == want } }

	It("finds an object on a later page and asks for no more pages", func() {
		list, asked := pages(2, []string{"a", "b"}, []string{"c", "d"}, []string{"e"})
		found, err := findInPages(list, is("c"))
		Expect(err).NotTo(HaveOccurred())
		Expect(*found).To(Equal("c"))
		Expect(*asked).To(Equal([]int64{1, 2}))
	})

	It("stops at a page that holds fewer objects than the page size", func() {
		list, asked := pages(2, []string{"a", "b"}, []string{"c"})
		found, err := findInPages(list, is("z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeNil())
		Expect(*asked).To(Equal([]int64{1, 2}))
	})

	It("stops at an empty page after full ones", func() {
		list, asked := pages(2, []string{"a", "b"})
		found, err := findInPages(list, is("z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeNil())
		Expect(*asked).To(Equal([]int64{1, 2}))
	})

	It("keeps paging when Cloudflare reports no page size, until a page is empty", func() {
		list, asked := pages(0, []string{"a"}, []string{"b"})
		found, err := findInPages(list, is("z"))
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeNil())
		Expect(*asked).To(Equal([]int64{1, 2, 3}))
	})

	It("returns the error of a page", func() {
		failing := func(int64) (*pagination.V4PagePaginationArray[string], error) {
			return nil, errors.New("unavailable")
		}
		_, err := findInPages(failing, is("a"))
		Expect(err).To(MatchError("unavailable"))
	})
})
