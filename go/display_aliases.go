package main

import "copilot-token-cost/internal/display"

var (
	addCommas     = display.AddCommas
	cacheHitPct   = display.CacheHitPct
	commaFloat    = display.CommaFloat
	commaInt      = display.CommaInt
	displayWidth  = display.DisplayWidth
	fmtCost       = display.FmtCost
	fmtTokens     = display.FmtTokens
	isWideRune    = display.IsWideRune
	padCell       = display.PadCell
	printSQLJSON  = display.PrintSQLJSON
	printSQLTable = display.PrintSQLTable
	printTable    = display.PrintTable
	roundN        = display.RoundN
)
