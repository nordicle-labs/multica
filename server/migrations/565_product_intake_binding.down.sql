DROP TRIGGER IF EXISTS trg_product_intake_delivery_immutable ON product_intake_delivery;
DROP TRIGGER IF EXISTS trg_product_intake_approval_immutable ON product_intake_approval;
DROP FUNCTION IF EXISTS reject_product_intake_immutable_update();
DROP TABLE IF EXISTS product_intake_delivery;
DROP TABLE IF EXISTS product_intake_approval;
DROP TABLE IF EXISTS product_intake_binding;
