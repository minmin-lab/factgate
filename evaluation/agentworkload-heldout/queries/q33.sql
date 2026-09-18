
SELECT row_id, event_timestamp, processed_at
FROM final_v5_result_heavy
WHERE NOT (event_timestamp <= processed_at)
ORDER BY row_id;

