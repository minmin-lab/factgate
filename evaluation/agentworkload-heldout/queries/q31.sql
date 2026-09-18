SELECT row_id, category, quantity
FROM final_v5_result_heavy
WHERE region = 'AMER' AND revision IN (1, 3);
