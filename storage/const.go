/*
 * Copyright 2014 Canonical Ltd.
 *
 * Authors:
 * Sergio Schvezov: sergio.schvezov@cannical.com
 *
 * This file is part of telepathy.
 *
 * mms is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; version 3.
 *
 * mms is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package storage

const (
	NONE          = "none"
	EXPIRED       = "expired"
	RETRIEVED     = "retrieved"
	REJECTED      = "rejected"
	DEFERRED      = "deferred"
	INDETERMINATE = "indeterminate"
	FORWARDED     = "forwarded"
	UNREACHABLE   = "unreachable"
)

const (
	NOTIFICATION = "notification"
	DOWNLOADED   = "downloaded"
	RECEIVED     = "received"
	RESPONDED    = "responded"
	DRAFT        = "draft"
	SENT         = "sent"
)
