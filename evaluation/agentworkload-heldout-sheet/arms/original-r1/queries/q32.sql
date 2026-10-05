```sql
SELECT row_id, amount, tax_amount, amount + tax_amount AS amount_incl_tax
FROM final_v5_result_heavy
WHERE row_id <= 2000
ORDER BY row_id;
```
