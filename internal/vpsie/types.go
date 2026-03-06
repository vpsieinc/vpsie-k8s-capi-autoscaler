package vpsie

// PlanCategory represents a VPSie plan category from GET /api/v2/plans/category.
type PlanCategory struct {
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
}

// Plan represents a VPSie VM plan from POST /api/v2/resources.
type Plan struct {
	Identifier   string  `json:"identifier"`
	Nickname     string  `json:"nickname"`
	CPU          int     `json:"cpu"`
	RAM          int     `json:"ram"`     // MB
	SSD          int     `json:"ssd"`     // GB
	Traffic      int     `json:"traffic"` // GB
	PriceMonthly float64 `json:"price"`
	CategoryID   string  `json:"categoryId"`
	CategoryName string  `json:"categoryName"`
}

// categoriesResponse wraps the API response for plan categories.
type categoriesResponse struct {
	Data []PlanCategory `json:"data"`
}

// plansResponse wraps the API response for plans/resources.
type plansResponse struct {
	Data []planData `json:"data"`
}

type planData struct {
	Identifier string  `json:"identifier"`
	Nickname   string  `json:"nickname"`
	CPU        int     `json:"cpu"`
	RAM        int     `json:"ram"`
	SSD        int     `json:"ssd"`
	Traffic    int     `json:"traffic"`
	Price      float64 `json:"price"`
}
