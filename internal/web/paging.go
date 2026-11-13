package web

import "strconv"

// Page 分页计算：页码钳制与页码窗口，前台后台共用。
type Page struct {
	Number int
	Size   int
	Total  int
}

// NewPage 按总数与页大小造一个钳制过的 Page。
func NewPage(raw string, size, total int) Page {
	number, err := strconv.Atoi(raw)
	if err != nil || number < 1 {
		number = 1
	}
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if number > pages {
		number = pages
	}
	return Page{Number: number, Size: size, Total: total}
}

// Pages 总页数。
func (p Page) Pages() int {
	if p.Size <= 0 {
		return 1
	}
	n := (p.Total + p.Size - 1) / p.Size
	if n < 1 {
		return 1
	}
	return n
}

// HasPrev 是否有上一页。
func (p Page) HasPrev() bool { return p.Number > 1 }

// HasNext 是否有下一页。
func (p Page) HasNext() bool { return p.Number < p.Pages() }

// Prev 上一页页码。
func (p Page) Prev() int {
	if p.Number <= 1 {
		return 1
	}
	return p.Number - 1
}

// Next 下一页页码。
func (p Page) Next() int {
	if p.Number >= p.Pages() {
		return p.Pages()
	}
	return p.Number + 1
}

// Window 当前页前后各 span 页的页码窗口。
func (p Page) Window(span int) []int {
	low := p.Number - span
	if low < 1 {
		low = 1
	}
	high := p.Number + span
	if high > p.Pages() {
		high = p.Pages()
	}
	out := make([]int, 0, high-low+1)
	for i := low; i <= high; i++ {
		out = append(out, i)
	}
	return out
}
