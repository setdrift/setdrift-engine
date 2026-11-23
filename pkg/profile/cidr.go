// finds the smallest range holding a set of addresses or ranges, how the profile sizes the networks it only sees in use

package profile

import (
	"net/netip"
	"slices"
)

// covering takes addresses and ranges, an address as a range of one, and gives back the smallest range holding all of them, one per address family
// a family with nothing in it gives nothing, so the result has at most one IPv4 and one IPv6 range
func covering(prefixes []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	// going through the two families, IPv4 first
	for _, v6 := range []bool{false, true} {
		var family []netip.Prefix
		// going through the ranges, keeping this family's
		for _, p := range prefixes {
			// checking if the range is this family
			if p.IsValid() && p.Addr().Is6() == v6 {
				family = append(family, p.Masked())
			}
		}
		// checking if the family has any
		if len(family) == 0 {
			continue
		}
		// invoking cover to find the smallest range holding them, its defined below
		out = append(out, cover(family))
	}
	return out
}

// cover takes ranges of one family and gives back the smallest range holding all of them
// it starts from the first range and widens it a bit at a time until every other range fits inside
func cover(family []netip.Prefix) netip.Prefix {
	bits := family[0].Bits()
	// going through the ranges, starting from the widest one as no range smaller than that can hold it
	for _, p := range family {
		bits = min(bits, p.Bits())
	}
	// widening one bit at a time
	for ; bits > 0; bits-- {
		c := netip.PrefixFrom(family[0].Addr(), bits).Masked()
		// checking if every range sits inside this one, if so its the smallest that holds them all
		if !slices.ContainsFunc(family, func(p netip.Prefix) bool { return !c.Contains(p.Addr()) || p.Bits() < bits }) {
			return c
		}
	}
	return netip.PrefixFrom(family[0].Addr(), 0).Masked()
}

// Copyright The SetDrift Authors
// SPDX-License-Identifier: Apache-2.0
