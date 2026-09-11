package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

func ReportTitle(s string, w int) string {
	quoted := strconv.Quote(s)
	if DisplayWidth(quoted) <= w {
		return quoted
	}

	room := w - 2
	if room <= 0 {
		return ""
	}

	mark := ellipsis
	if room <= len(ellipsis) {
		mark = ""
	}
	return `"` + escapeCut(s, room-len(mark), quotePiece) + mark + `"`
}

const minForeignWidth = 8

func Foreign(s string, w int) string {
	if w < minForeignWidth {
		w = minForeignWidth
	}
	return ReportTitle(s, w)
}

func escape(s string) string {
	return escapeCut(s, noBudget, cellPiece)
}

func escapeCell(s string, w int) string {
	escaped := escape(s)
	if DisplayWidth(escaped) <= w {
		return escaped
	}
	if w <= 0 {
		return ""
	}

	mark := ellipsis
	if w <= len(ellipsis) {
		mark = ""
	}
	return escapeCut(s, w-len(mark), cellPiece) + mark
}

func escapeBlock(s string) string {
	return escapeCut(strings.ReplaceAll(s, "\r\n", "\n"), noBudget, blockPiece)
}

const noBudget = -1

func escapeCut(s string, budget int, piece func(string) string) string {
	var b strings.Builder
	graphemes := uniseg.NewGraphemes(s)
	for graphemes.Next() {
		p := renderCluster(graphemes.Str(), piece)
		if budget != noBudget {
			if budget -= DisplayWidth(p); budget < 0 {
				break
			}
		}
		b.WriteString(p)
	}
	return b.String()
}

func renderCluster(cluster string, piece func(string) string) string {
	var b strings.Builder
	for i := 0; i < len(cluster); {
		_, size := utf8.DecodeRuneInString(cluster[i:])
		b.WriteString(piece(cluster[i : i+size]))
		i += size
	}
	return b.String()
}

func quotePiece(chunk string) string {
	quoted := strconv.Quote(chunk)
	return quoted[1 : len(quoted)-1]
}

func blockPiece(chunk string) string {
	if chunk == "\n" {
		return chunk
	}
	return cellPiece(chunk)
}

func cellPiece(chunk string) string {
	if chunk == `"` || chunk == `\` {
		return chunk
	}
	return quotePiece(chunk)
}

func ReportLine(before, title, after string, style func(string) string) string {
	name := ReportTitle(title, Width-DisplayWidth(before)-DisplayWidth(after))
	if style != nil {
		name = style(name)
	}
	return before + name + after
}
