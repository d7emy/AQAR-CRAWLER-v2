package main

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Magic header for dictionary-deflate compressed JSON blobs.
// Any blob starting with "AQZ1" is compressed with flate using aqarFlateDict.
// Any blob starting with '{' is plain uncompressed JSON (backward-compatible).
var aqz1Magic = []byte{'A', 'Q', 'Z', '1'}

// aqarFlateDict is a deterministic preset DEFLATE dictionary tailored to
// canonical (alphabetically key-sorted) AQAR GraphQL listing JSON payloads.
// IMPORTANT: Never alter this dictionary once written, or bump the magic header
// version (e.g. AQZ2), because existing AQZ1 blobs depend on these exact bytes.
var aqarFlateDict = buildAQARDict()

func buildAQARDict() []byte {
	// Construct 4 canonical JSON variations covering the common AQAR listing
	// shapes so that DEFLATE finds long contiguous runs of matching keys+values
	// regardless of whether alert_box, extended_details, or array jitter is set.
	// The most common shape (Template 4: extended_details populated, alert_box null)
	// is placed last in the 32KB sliding window for minimum back-reference distance.
	const extDetails = `{"ac_type":null,"accept_bank":null,"ad_source":"عقار","apartment_in_project":null,"apartments_count":null,"balcony":null,"bedrooms_count":null,"capacity":null,"cctv":null,"channels":"منصة مرخصة","check_in_hour":13,"check_out_hour":12,"cleaning_service":null,"compliance_with_the_saudi_building_code":null,"daily_renting_booking_conditions":null,"daily_renting_grace_period":0,"driversrooms_count":null,"end_date":"/2026","external_area":null,"fiber_optic":null,"floors_count":null,"grill":null,"guarantees_and_their_duration":"لا يوجد","guestsrooms_count":null,"heater":null,"instant_booking":null,"insurance":null,"internal_area":null,"iron":null,"is_constrained":false,"is_pawned":false,"last_time_to_book":24,"laundry_room":null,"location_description_on_moj_deed":null,"lower_extension":null,"master_bedroom":null,"master_bedroom_count":null,"microphone":null,"microwave":null,"minimum_booking_days":0,"national_address":"{\"region\":\"منطقة الرياض\",\"regionCode\":\"1\",\"city\":\"الرياض\",\"cityCode\":\"11\",\"district\":\"حي \",\"districtCode\":\"10\",\"street\":\"شارع \",\"postalCode\":\"1\",\"buildingNumber\":\"\",\"additionalNumber\":\"\",\"longitude\":\"46.\",\"latitude\":\"24.\",\"districtId\":\"-05ec-11ef-a633-02c538233d1b\",\"cityId\":\"-0e7a-e01f-449f-d055356c9b21\",\"regionId\":\"-22f6-59d5-f60c-f8b607c1d078\"}","national_address_additional_number":"","national_address_building_number":"","national_address_city":"الرياض","national_address_district":"حي ","national_address_postal_code":"","national_address_region":"منطقة الرياض","national_address_street":"شارع ","notes":null,"obligations":"لا يوجد ","obligations_on_the_property":"لا يوجد ","other_rooms_count":null,"outdoor_cooling_system":null,"oven":null,"ownership_transfer_fee_type":null,"private_beach":null,"project_name":null,"projector":null,"property_usages_list":"","raw_land":null,"responsible_employee_name":"","responsible_employee_phone_number":"مرخص عن طريق تطبيق عقار، اضغط على زر الاتصال أو الواتساب للتواصل","roof_villa":null,"saudi_code_compliance":null,"self_entry":null,"separated_electrical_meter":null,"separated_water_meter":null,"shampoo":null,"sleeping":null,"smart_tv":null,"smoking":null,"sound_system":null,"special_parking":null,"special_view":null,"towels":null,"upper_extension":null,"washing_machine":null,"wifi":null}`

	const alertBox = `{"for_listing_owner":{"background_color":"#","border_color":"#","msg_ar":"","msg_en":"","next_action":null,"show":false,"text_color":"#"},"for_searcher":{"background_color":"#","border_color":"#","msg_ar":"","msg_en":"","next_action":null,"show":false,"text_color":"#"},"for_subscriber":{"background_color":"#","border_color":"#","msg_ar":"","msg_en":"","next_action":null,"show":false,"text_color":"#"}}`

	mkTemplate := func(ab, bup, ed, idesc string) string {
		return `{"ac":null,"accept_monthly":null,"accept_quarterly":null,"accept_semiannually":null,"ad_license_number":"7200","address":"شارع ","age":0,"alert_box":` + ab + `,"apartment_in_villa":null,"apts":null,"area":300,"backyard":null,"basement":null,"beds":null,"bookable_unit":false,"bookable_unit_interested_users_count":null,"bookable_unit_plans":` + bup + `,"boosted":0,"car_entrance":null,"category":1,"cf_videos":null,"city_id":21,"comments_enabled":true,"commission_amount":2.5,"commission_percentage":0.1,"commission_type":"percentage","content":"السلام عليكم ورحمة الله وبركاته ","create_time":178,"daily_rentable":false,"daily_renting_discount_reason":null,"deed_area":300,"direction_id":4,"disable_message":null,"disable_phone":null,"disable_whatsapp":null,"district_id":500,"document_pdf":null,"drainage_availability":true,"driver":null,"duplex":null,"electrical_availability":true,"employee":null,"employee_user_id":null,"extended_details":` + ed + `,"extra_unit":null,"family":null,"family_section":null,"fb":null,"fl":null,"furnished":0,"has_extended_details":true,"id":6000000,"imgs":["0",".jpg",".webp"],"imgs_desc":` + idesc + `,"is_auction":null,"ketchen":1,"lift":null,"livings":1,"location":{"lat":24.7,"lng":46.7},"maid":null,"map_premium_services":null,"men_place":null,"meter_price":null,"mot_license_number":null,"near_bus":false,"near_metro":false,"original_range_price":null,"parcel_no":"بدون","plan_no":"بدون","playground":null,"pool":null,"precise_location_request_approved_at":null,"precise_location_request_status":null,"premium":0,"price":50000,"price_12_payments":null,"price_2_payments":null,"price_4_payments":null,"private_roof":null,"project_id":null,"province_id":1,"published":true,"range_price":null,"refresh":1787,"rega_licensed":true,"rega_meter_price":null,"rega_total_price":null,"rent_period":3,"rnpl_monthly_price":null,"rooms":4,"sov_campaign_id":null,"special_entrance":null,"stairs":null,"status":0,"stores":null,"street_direction":1,"street_width":15,"tent":null,"trees":null,"two_entrances":null,"type":1,"unit_code":null,"usages":["سكني","تجاري"],"user":{"company_logo":null,"company_name":null,"fee":null,"iam_verified":true,"img":null,"name":"مكتب عقار","paid":0,"review":4.5,"type":0},"user_id":1000000,"vb":null,"videos":null,"villa_type":null,"virtual_tour_link":null,"water_availability":true,"wc":3,"wells":null,"whatsapp_requested_count":10,"whatsapp_responded_count":10,"women_place":null}`
	}

	const locales = `{"cf_videos":[{"playback":{"dash":"https://customer-","hls":"https://customer-"},"readyToStream":true,"thumbnail":"https://customer-"}],"videos":[{"thumbnail":".jpg","video":".mp4"}],"map_premium_services":[{"distance_in_km":0.5,"logo":".png","name":"جامع "},{"distance_in_km":1.2,"logo":".png","name":"مدارس "}],"regions":["منطقة الرياض","منطقة مكة المكرمة","منطقة المنطقة الشرقية","منطقة المدينة المنورة","منطقة القصيم","منطقة عسير","منطقة تبوك","منطقة حائل","منطقة جازان","منطقة نجران","منطقة الباحة","منطقة الجوف","منطقة الحدود الشمالية","جدة","مكة المكرمة","الدمام","الخبر","المدينة المنورة","بريدة","خميس مشيط","أبها","الطائف","تبوك","حائل","الجبيل","الخرج","الأحساء","الهفوف","عنيزة","ينبع","حفر الباطن"]}`

	var buf bytes.Buffer
	buf.WriteString(locales)
	buf.WriteString(mkTemplate(alertBox, `[]`, `null`, `null`))
	buf.WriteString(mkTemplate(`null`, `[""]`, extDetails, `[""]`))
	buf.WriteString(mkTemplate(`null`, `[]`, extDetails, `null`))
	b := buf.Bytes()
	if len(b) > 32768 {
		b = b[len(b)-32768:]
	}
	return b
}

var flateWriterPool = sync.Pool{
	New: func() any {
		w, _ := flate.NewWriterDict(io.Discard, flate.BestCompression, aqarFlateDict)
		return w
	},
}

var flateReaderPool = sync.Pool{
	New: func() any {
		return flate.NewReaderDict(bytes.NewReader(nil), aqarFlateDict)
	},
}

var byteBufPool = sync.Pool{
	New: func() any {
		b := new(bytes.Buffer)
		b.Grow(8192)
		return b
	},
}

// CompressJSON compresses a raw JSON byte slice using DEFLATE with aqarFlateDict
// and prefixes it with the 4-byte "AQZ1" magic header.
// It is 100% lossless: DecompressJSON restores the exact input byte-for-byte.
func CompressJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	if IsCompressedJSON(raw) {
		return raw
	}
	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Write(aqz1Magic)

	w := flateWriterPool.Get().(*flate.Writer)
	w.Reset(buf)
	_, _ = w.Write(raw)
	_ = w.Close()
	flateWriterPool.Put(w)

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	byteBufPool.Put(buf)
	return out
}

// IsCompressedJSON reports whether data carries the AQZ1 compressed header.
func IsCompressedJSON(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == 'A' && data[1] == 'Q' && data[2] == 'Z' && data[3] == '1'
}

// DecompressJSONBytes transparently decompresses an AQZ1 blob into a byte slice
// or returns plain-text JSON bytes unchanged if it is not compressed.
func DecompressJSONBytes(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if !IsCompressedJSON(data) {
		return data, nil
	}
	r := flateReaderPool.Get().(io.ReadCloser)
	_ = r.(flate.Resetter).Reset(bytes.NewReader(data[4:]), aqarFlateDict)

	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, err := buf.ReadFrom(r)
	_ = r.Close()
	flateReaderPool.Put(r)
	if err != nil {
		byteBufPool.Put(buf)
		return nil, fmt.Errorf("decompress AQZ1 json: %w", err)
	}
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	byteBufPool.Put(buf)
	return out, nil
}

// DecompressJSON transparently decompresses an AQZ1 blob or returns plain-text
// JSON unchanged if it is not compressed.
func DecompressJSON(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if !IsCompressedJSON(data) {
		return string(data), nil
	}
	r := flateReaderPool.Get().(io.ReadCloser)
	_ = r.(flate.Resetter).Reset(bytes.NewReader(data[4:]), aqarFlateDict)

	buf := byteBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, err := buf.ReadFrom(r)
	_ = r.Close()
	flateReaderPool.Put(r)
	if err != nil {
		byteBufPool.Put(buf)
		return "", fmt.Errorf("decompress AQZ1 json: %w", err)
	}
	s := buf.String()
	byteBufPool.Put(buf)
	return s, nil
}

// CanonicalContentHash computes the noise-resistant SHA-256 content hash of a
// listing map by marshaling it canonically and hashing via CanonicalContentHashBytes.
func CanonicalContentHash(m map[string]any) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return CanonicalContentHashBytes(raw), nil
}

// volatileKeyTokens are JSON key tokens (preceded by ',' or '{') whose scalar
// value (number or null) is normalized to "null" during content hashing so live
// view/bump/ad-rotation counters never trigger fake history update rows.
var volatileKeyTokens = [][]byte{
	[]byte(`"bookable_unit_interested_users_count":`),
	[]byte(`"paid":`),
	[]byte(`"refresh":`),
	[]byte(`"review":`),
	[]byte(`"sov_campaign_id":`),
	[]byte(`"whatsapp_requested_count":`),
	[]byte(`"whatsapp_responded_count":`),
}

// jitterArrayKeyTokens are JSON key tokens whose empty-array variants
// (null, [], [""], ["",""]) are normalized to "null" during content hashing.
var jitterArrayKeyTokens = [][]byte{
	[]byte(`"bookable_unit_plans":`),
	[]byte(`"imgs_desc":`),
}

// CanonicalContentHashBytes computes a deterministic SHA-256 hash of canonical
// JSON bytes in a single zero-allocation pass, normalizing AQAR backend array
// jitter (null vs [] vs [""]) and volatile engagement/bump counters.
func CanonicalContentHashBytes(raw []byte) string {
	h := sha256.New()
	n := len(raw)
	lastWrite := 0
	i := 0

	for i < n {
		// Keys in canonical JSON always follow '{' or ','
		if raw[i] != '{' && raw[i] != ',' {
			i++
			continue
		}
		keyStart := i + 1
		if keyStart >= n || raw[keyStart] != '"' {
			i++
			continue
		}

		// Check jitterArrayKeyTokens ("bookable_unit_plans":, "imgs_desc":)
		matched := false
		for _, tok := range jitterArrayKeyTokens {
			if keyStart+len(tok) <= n && bytes.Equal(raw[keyStart:keyStart+len(tok)], tok) {
				valStart := keyStart + len(tok)
				if end, ok := skipEmptyArrayOrNull(raw, valStart); ok {
					_, _ = h.Write(raw[lastWrite:valStart])
					_, _ = h.Write([]byte("null"))
					lastWrite = end
					i = end
					matched = true
				}
				break
			}
		}
		if matched {
			continue
		}

		// Check volatileKeyTokens ("refresh":, "whatsapp_*":, etc.)
		for _, tok := range volatileKeyTokens {
			if keyStart+len(tok) <= n && bytes.Equal(raw[keyStart:keyStart+len(tok)], tok) {
				valStart := keyStart + len(tok)
				if end, ok := skipScalarNumOrNull(raw, valStart); ok {
					_, _ = h.Write(raw[lastWrite:valStart])
					_, _ = h.Write([]byte("null"))
					lastWrite = end
					i = end
					matched = true
				}
				break
			}
		}
		if !matched {
			i = keyStart + 1
		}
	}

	if lastWrite < n {
		_, _ = h.Write(raw[lastWrite:])
	}
	var sum [32]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}

// skipEmptyArrayOrNull checks if raw[pos:] starts with `null`, `[]`, or an
// array containing only empty strings/nulls (e.g. `[""]`, `["",""]`), and
// returns the index immediately after it.
func skipEmptyArrayOrNull(raw []byte, pos int) (int, bool) {
	n := len(raw)
	if pos+4 <= n && string(raw[pos:pos+4]) == "null" {
		return pos + 4, true
	}
	if pos >= n || raw[pos] != '[' {
		return 0, false
	}
	j := pos + 1
	for j < n {
		switch raw[j] {
		case ']':
			return j + 1, true
		case ',':
			j++
		case '"':
			// Only empty string "" is considered jitter; non-empty string is real data
			if j+1 < n && raw[j+1] == '"' {
				j += 2
			} else {
				return 0, false
			}
		case 'n':
			if j+4 <= n && string(raw[j:j+4]) == "null" {
				j += 4
			} else {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	return 0, false
}

// skipScalarNumOrNull checks if raw[pos:] starts with `null`, a boolean, a JSON
// string, or a JSON number token ending at `,` or `}`, and returns the index
// immediately after the token.
func skipScalarNumOrNull(raw []byte, pos int) (int, bool) {
	n := len(raw)
	if pos >= n {
		return 0, false
	}
	if pos+4 <= n && (string(raw[pos:pos+4]) == "null" || string(raw[pos:pos+4]) == "true") {
		return pos + 4, true
	}
	if pos+5 <= n && string(raw[pos:pos+5]) == "false" {
		return pos + 5, true
	}
	if raw[pos] == '"' {
		j := pos + 1
		for j < n {
			if raw[j] == '\\' {
				j += 2
				continue
			}
			if raw[j] == '"' {
				return j + 1, true
			}
			j++
		}
		return 0, false
	}
	j := pos
	for j < n {
		c := raw[j]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
			j++
			continue
		}
		break
	}
	if j > pos && j < n && (raw[j] == ',' || raw[j] == '}') {
		return j, true
	}
	return 0, false
}
