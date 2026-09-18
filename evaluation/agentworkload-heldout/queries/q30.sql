
SELECT row_id, category, amount, event_date, sequence_no, approved, event_timestamp, description, quantity, unit_price, tax_amount, settled_date, processed_at, region, revision, active
FROM final_v5_result_heavy
WHERE approved = true
ORDER BY amount DESC
LIMIT 50

