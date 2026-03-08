package display

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode"
)

func CacheHitPct(promptTokens, cacheReadTokens int) string {
	if promptTokens == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", float64(cacheReadTokens)/float64(promptTokens)*100)
}

func FmtTokens(n int) string {
	if n >= 1_000_000 {
		return CommaFloat(float64(n)/1e6, 1) + "M"
	}
	if n >= 1_000 {
		return CommaFloat(float64(n)/1e3, 1) + "K"
	}
	return strconv.Itoa(n)
}

func CommaFloat(f float64, decimals int) string {
	format := fmt.Sprintf("%%.%df", decimals)
	s := fmt.Sprintf(format, f)
	return AddCommas(s)
}

func AddCommas(s string) string {
	dotIdx := strings.Index(s, ".")
	intPart := s
	decPart := ""
	if dotIdx >= 0 {
		intPart = s[:dotIdx]
		decPart = s[dotIdx:]
	}
	negative := false
	if strings.HasPrefix(intPart, "-") {
		negative = true
		intPart = intPart[1:]
	}
	var result []byte
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	prefix := ""
	if negative {
		prefix = "-"
	}
	return prefix + string(result) + decPart
}

func FmtCost(cost float64) string {
	if cost >= 100 {
		return "$" + AddCommas(fmt.Sprintf("%.0f", cost))
	}
	if cost >= 1 {
		return "$" + AddCommas(fmt.Sprintf("%.2f", cost))
	}
	return "$" + AddCommas(fmt.Sprintf("%.3f", cost))
}

func CommaInt(n int) string {
	return AddCommas(strconv.Itoa(n))
}

func DisplayWidth(s string) int {
	runes := []rune(s)
	w := 0
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if i+1 < len(runes) && runes[i+1] == '\uFE0F' {
			w += 2
			i++
			continue
		}
		if unicode.In(ch, unicode.Mn, unicode.Mc, unicode.Me) {
			continue
		}
		if IsWideRune(ch) {
			w += 2
		} else {
			w++
		}
	}
	return w
}

func IsWideRune(r rune) bool {
	if r >= 0x1100 && r <= 0x115F {
		return true
	}
	if r >= 0x2E80 && r <= 0x303E {
		return true
	}
	if r >= 0x3040 && r <= 0x33BF {
		return true
	}
	if r >= 0x3400 && r <= 0x4DBF {
		return true
	}
	if r >= 0x4E00 && r <= 0xA4CF {
		return true
	}
	if r >= 0xA960 && r <= 0xA97C {
		return true
	}
	if r >= 0xAC00 && r <= 0xD7FF {
		return true
	}
	if r >= 0xF900 && r <= 0xFAFF {
		return true
	}
	if r >= 0xFE10 && r <= 0xFE6F {
		return true
	}
	if r >= 0xFF01 && r <= 0xFF60 {
		return true
	}
	if r >= 0xFFE0 && r <= 0xFFE6 {
		return true
	}
	if r >= 0x1F000 && r <= 0x1FFFF {
		return true
	}
	if r >= 0x20000 && r <= 0x2FFFF {
		return true
	}
	if r >= 0x30000 && r <= 0x3FFFF {
		return true
	}
	return false
}

func PadCell(cell string, width int, alignRight bool) string {
	dw := DisplayWidth(cell)
	padding := width - dw
	if padding < 0 {
		padding = 0
	}
	if alignRight {
		return strings.Repeat(" ", padding) + cell
	}
	return cell + strings.Repeat(" ", padding)
}

func PrintTable(title string, headers []string, rows [][]string, footer []string, notes []string) {
	allRows := [][]string{headers}
	allRows = append(allRows, rows...)
	if footer != nil {
		allRows = append(allRows, footer)
	}

	colWidths := make([]int, len(headers))
	for _, row := range allRows {
		for i, cell := range row {
			w := DisplayWidth(cell)
			if w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}

	innerWidth := 4
	for i, w := range colWidths {
		innerWidth += w
		if i > 0 {
			innerWidth += 2
		}
	}

	fmtRow := func(cells []string) string {
		var parts []string
		for i, cell := range cells {
			parts = append(parts, PadCell(cell, colWidths[i], i > 0))
		}
		content := "  " + strings.Join(parts, "  ")
		contentWidth := DisplayWidth(content)
		pad := innerWidth - contentWidth
		if pad < 0 {
			pad = 0
		}
		return "│" + content + strings.Repeat(" ", pad) + "│"
	}

	separator := func() string {
		var parts []string
		for _, w := range colWidths {
			parts = append(parts, strings.Repeat("─", w))
		}
		content := "  " + strings.Join(parts, "  ") + "  "
		pad := innerWidth - len(content)
		if pad < 0 {
			pad = 0
		}
		return "│" + content + strings.Repeat(" ", pad) + "│"
	}

	bar := strings.Repeat("─", innerWidth)
	titlePad := innerWidth - DisplayWidth(title) - 3
	if titlePad < 0 {
		titlePad = 0
	}

	fmt.Printf("┌─ %s %s┐\n", title, strings.Repeat("─", titlePad))
	fmt.Printf("│%s│\n", strings.Repeat(" ", innerWidth))
	fmt.Println(fmtRow(headers))
	fmt.Println(separator())
	for _, row := range rows {
		fmt.Println(fmtRow(row))
	}
	if footer != nil {
		fmt.Println(separator())
		fmt.Println(fmtRow(footer))
	}
	fmt.Printf("│%s│\n", strings.Repeat(" ", innerWidth))
	fmt.Printf("└%s┘\n", bar)
	for _, note := range notes {
		fmt.Printf("  %s\n", note)
	}
}

func PrintSQLTable(cols []string, rows *sql.Rows) {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	w.WriteString(strings.Join(cols, "\t") + "\n")
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		rows.Scan(ptrs...)
		for i, v := range vals {
			if i > 0 {
				w.WriteByte('\t')
			}
			if v.Valid {
				w.WriteString(v.String)
			} else {
				w.WriteString("NULL")
			}
		}
		w.WriteByte('\n')
	}
}

func PrintSQLJSON(cols []string, rows *sql.Rows) {
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var result []map[string]interface{}
	for rows.Next() {
		rows.Scan(ptrs...)
		row := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			if vals[i].Valid {
				row[c] = vals[i].String
			} else {
				row[c] = nil
			}
		}
		result = append(result, row)
	}
	if result == nil {
		result = []map[string]interface{}{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
}

func RoundN(f float64, n int) float64 {
	pow := math.Pow(10, float64(n))
	return math.Round(f*pow) / pow
}
