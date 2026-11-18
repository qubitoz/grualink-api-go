package grualink

// Request and response shapes of the public API. JSON names are the wire names (snake_case).

// Float64 returns a pointer to v. Use it for the optional coordinates of a Place.
func Float64(v float64) *float64 { return &v }

// Place is a pickup or destination point.
type Place struct {
	// Address is required, up to 500 characters.
	Address string `json:"address"`
	// Lat and Lng go together. Without pickup and destination coordinates there is no estimated price.
	Lat *float64 `json:"lat,omitempty"`
	Lng *float64 `json:"lng,omitempty"`
}

// Contact is the person the operator will call.
type Contact struct {
	// Name has up to 120 characters.
	Name string `json:"name"`
	// Phone is in international format (+52...); 10 digits are taken as a Mexican number.
	Phone string `json:"phone"`
}

// Vehicle describes the vehicle to tow. Every field is optional.
type Vehicle struct {
	Make  string `json:"make,omitempty"`
	Model string `json:"model,omitempty"`
	Year  int    `json:"year,omitempty"`
	Color string `json:"color,omitempty"`
	Plate string `json:"plate,omitempty"`
	// Type is one of: auto, camioneta, motocicleta, camion, otro.
	Type string `json:"type,omitempty"`
}

// ServiceDetails describes what is needed. Every field is optional.
type ServiceDetails struct {
	Reason string `json:"reason,omitempty"`
	// Category is one of: vial, colision.
	Category string `json:"category,omitempty"`
	// RequiredClass is one of: A, B, C, D.
	RequiredClass string `json:"required_class,omitempty"`
	// RequiredAccessories has up to 12 values, without duplicates: dolly, winch, cadenas, eslingas,
	// patines, rampas, barra_tiro, extension_pluma, gato_hidraulico, pasacorriente, compresor,
	// luces_remolque.
	RequiredAccessories []string `json:"required_accessories,omitempty"`
}

// ServiceRequestInput is the body of POST /service-requests. Unknown fields are rejected by the API
// with validation_error.
type ServiceRequestInput struct {
	// ExternalRef is your own reference (claim or case number), up to 64 characters.
	ExternalRef string          `json:"external_ref,omitempty"`
	Contact     Contact         `json:"contact"`
	Vehicle     *Vehicle        `json:"vehicle,omitempty"`
	Pickup      Place           `json:"pickup"`
	Destination *Place          `json:"destination,omitempty"`
	Service     *ServiceDetails `json:"service,omitempty"`
	// Notes has up to 1,000 characters and 20 lines.
	Notes string `json:"notes,omitempty"`
}

// ServiceRequestStatus is the status of a service request.
type ServiceRequestStatus string

// Statuses of a service request.
const (
	StatusReceived   ServiceRequestStatus = "received"
	StatusQuoted     ServiceRequestStatus = "quoted"
	StatusConfirmed  ServiceRequestStatus = "confirmed"
	StatusInProgress ServiceRequestStatus = "in_progress"
	StatusCompleted  ServiceRequestStatus = "completed"
	StatusRejected   ServiceRequestStatus = "rejected"
	StatusCancelled  ServiceRequestStatus = "cancelled"
	StatusExpired    ServiceRequestStatus = "expired"
)

// Tax is one line of the estimated price.
type Tax struct {
	Name string `json:"name"`
	// Type is tax (added) or retention (subtracted).
	Type   string  `json:"type"`
	Rate   float64 `json:"rate"`
	Amount float64 `json:"amount"`
}

// EstimatedPrice is the estimate of a quote.
type EstimatedPrice struct {
	Currency       string  `json:"currency"`
	Subtotal       float64 `json:"subtotal"`
	Taxes          []Tax   `json:"taxes"`
	TaxTotal       float64 `json:"tax_total"`
	RetentionTotal float64 `json:"retention_total"`
	Total          float64 `json:"total"`
}

// Quote is the quote of a service request.
type Quote struct {
	// EstimatedPrice is nil when the request could not be priced automatically; see UnpricedReason.
	EstimatedPrice *EstimatedPrice `json:"estimated_price"`
	// IncludesExtras is always false: the estimate never includes extras such as tolls.
	IncludesExtras      bool `json:"includes_extras"`
	ApproximateDistance bool `json:"approximate_distance"`
	// UnpricedReason is one of: missing_coordinates, missing_destination, no_zones_configured,
	// missing_vehicle_class.
	UnpricedReason *string `json:"unpriced_reason"`
	Note           string  `json:"note"`
}

// RegisteredService is present once the company registered the service.
type RegisteredService struct {
	Folio  *string `json:"folio"`
	Status string  `json:"status"`
}

// Truck is the tow truck assigned to a service request.
type Truck struct {
	EconomicNumber *string `json:"economic_number"`
	Plate          *string `json:"plate"`
}

// Assignment is the operator and truck assigned. Live keys only.
type Assignment struct {
	OperatorFirstName *string `json:"operator_first_name"`
	Truck             *Truck  `json:"truck"`
}

// Timeline has the moments of the service that already happened. Live keys only.
type Timeline struct {
	OnTheWayAt         string `json:"on_the_way_at,omitempty"`
	OnSiteAt           string `json:"on_site_at,omitempty"`
	TransportStartedAt string `json:"transport_started_at,omitempty"`
	CompletedAt        string `json:"completed_at,omitempty"`
}

// ServiceRequest is the service request object. Unknown fields are ignored (forward compatible).
type ServiceRequest struct {
	ID string `json:"id"`
	// Object is always service_request.
	Object string               `json:"object"`
	Status ServiceRequestStatus `json:"status"`
	// Mode is test or live, from the key.
	Mode        string  `json:"mode"`
	ExternalRef *string `json:"external_ref"`
	// CreatedAt and ResolvedAt are ISO 8601 timestamps in UTC.
	CreatedAt  string  `json:"created_at"`
	ResolvedAt *string `json:"resolved_at"`
	// RejectionReason is set when Status is rejected.
	RejectionReason *string            `json:"rejection_reason"`
	Service         *RegisteredService `json:"service"`
	Quote           Quote              `json:"quote"`
	Assignment      *Assignment        `json:"assignment"`
	Timeline        *Timeline          `json:"timeline"`
}

// ServiceRequestList is the answer of GET /service-requests?external_ref=...
type ServiceRequestList struct {
	// Object is always list.
	Object string `json:"object"`
	// Data has up to 20 requests, most recent first.
	Data []ServiceRequest `json:"data"`
}

// HealthStatus is the answer of GET /health.
type HealthStatus struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// WebhookEventData is the data of a webhook delivery.
type WebhookEventData struct {
	// PreviousStatus is the previous status on status events; nil on assigned, on_the_way and on_site.
	PreviousStatus *ServiceRequestStatus `json:"previous_status,omitempty"`
	// ServiceRequest is the same representation as GET /service-requests/{id} at that moment.
	ServiceRequest *ServiceRequest `json:"service_request,omitempty"`
	// Message is only present on webhook.test.
	Message string `json:"message,omitempty"`
}

// WebhookEvent is the body of a webhook delivery.
type WebhookEvent struct {
	// ID is the same value as the X-GruaLink-Delivery header and on every retry. Use it to deduplicate.
	ID string `json:"id"`
	// Object is always event.
	Object string `json:"object"`
	// Type is the event, for example service_request.confirmed. Ignore the types you do not know.
	Type string `json:"type"`
	// Mode is test or live.
	Mode      string           `json:"mode"`
	CreatedAt string           `json:"created_at"`
	Data      WebhookEventData `json:"data"`
}
